import { after, afterEach, before, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import http from "node:http";
import os from "node:os";
import path from "node:path";
import { createHash, randomUUID } from "node:crypto";
import { execFileSync, spawn } from "node:child_process";
import type { ExecutorResult, RunContext } from "../src/executor.js";
import type { BoundaryPermit, CodexExecutionSafety } from "../src/harness.js";
import { CapturePathMismatchError, GitCache, type AttemptSeedOptions } from "../src/git.js";
import { LimitReachedError } from "../src/limit.js";
import { RunRunner, failOriginForReason, type ExecutorFactory } from "../src/runner.js";
import { CodexSessionStore } from "../src/codex/session-state.js";
import { formatAttemptId, parseAttemptPath } from "../src/attempt-path.js";
import {
  LiveAttemptRegistry,
  mintAttemptId,
  newRunAttempt,
  quiesceRunAttempt,
  setQuiescenceViewForTests,
  type ProcessQuiescenceState,
  type QuiesceRunOutcome,
  type QuiesceRunRequest,
} from "../src/run-quiescence.js";
import { RUN_ATTEMPT_ENV, RUN_CLONE_KEY_ENV } from "../src/worker-spawn-mark.js";
import { skillsPluginDir } from "../src/skills-plugin.js";
import { defaultGitleaksShim } from "./gitleaks-shim.js";
import { makeFakeProcRoot, plantUnreadableUnattributed, scopedRealView, withQuiescenceView } from "./fake-proc.js";
import { HERMETIC_VIEW, restoreHermeticView } from "./setup/hermetic-proc.js";
import { listenUnix, shortUnixSocket } from "./unix-socket.js";
import { REAL_PROCFS_DENIED, realProcfsSkip } from "./real-procfs.js";
import { nullLogger, recordingLogger, testGitCacheOptions, noProofReseed } from "./helpers.js";
import {
  api,
  client,
  fakeGitlab,
  fx,
  git,
  gitlabClaim,
  homeDir,
  installHarness,
  runnerWith,
  simulateCommittedWork,
} from "./runner-harness.js";

// issue #1783 M2 — per-attempt clone paths on a Docker-wired worker. Every execution attempt seeds a
// FRESH `<key>.attempt-<attemptId>` clone; a predecessor's path is never reseeded, executed in,
// moved or deleted by a successor, so a Docker create the daemon accepted for the predecessor and
// completes late (its bind-source mkdir) can only ever land in the predecessor's path. These tests
// drive the real runner, the real git layer and the real process reaper against a fake Docker
// daemon that outlives a simulated worker restart (a new RunRunner + GitCache on the same dataDir,
// with an empty live-attempt set).

installHarness();

const GIT_ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };
const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];
const HAS_PROCFS = process.platform === "linux";
/** The skip for a case that plants or reaps REAL processes: none, "Linux only", or (a sandbox that
 *  denies enumerating the proc root, issue #1863) the shared detector's recorded reason. */
const realTableSkip = (label: string): string | false =>
  !HAS_PROCFS ? "reads procfs (Linux only)" : realProcfsSkip(`clone-attempt-paths: ${label}`);

// ─── fake Docker daemon ────────────────────────────────────────────────────────────────────
//
// Like `docker run -v`, a create makes its missing bind-source directory — but a HELD create does
// that mkdir only when it completes, which is exactly the late-create hazard: the daemon accepted
// the request before the predecessor was torn down and materializes its bind after.

interface FakeDaemon {
  socket: string;
  containers: Map<string, { Id: string; Mounts: Array<{ Type: string; Source: string; Destination: string }> }>;
  /** When set, the NEXT create waits for this promise before it mkdirs its bind and commits. */
  hold: Promise<void> | undefined;
  close: () => Promise<void>;
}

async function startFakeDaemon(): Promise<FakeDaemon> {
  const { socket, dispose } = shortUnixSocket();
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
          if (held) await held;
          const mounts = (body.HostConfig?.Binds ?? []).map((b) => {
            const [src, dst] = b.split(":");
            fs.mkdirSync(src!, { recursive: true });
            return { Type: "bind", Source: src!, Destination: dst ?? "/" };
          });
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
  try {
    await listenUnix(server, socket);
  } catch (err) {
    dispose();
    throw err;
  }
  daemon.close = () =>
    new Promise<void>((r) => {
      server.closeAllConnections();
      server.close(() => {
        dispose();
        r();
      });
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

let daemon: FakeDaemon;
let daemonDir: string;
const orphans: number[] = [];

// The real reaper sees this file's own descendants plus its orphans (reparented away, so recorded
// in a pidfile), never the host's other processes.
const orphanPidDir = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-orphan-pids-"));
const orphanPidFile = path.join(orphanPidDir, "orphans.pids");
// issue #1863: where the proc root cannot be enumerated (the Landlock command sandbox) the real view
// is unreadable, so every proof over it is `unverified`. There the suites that plant or reap REAL
// processes skip through the shared detector (realTableSkip), and the rest, which have no real
// process in scope, run over the hermetic empty fake root: for them both views list nothing.
const FILE_VIEW = REAL_PROCFS_DENIED ? HERMETIC_VIEW : scopedRealView({ pidFiles: [orphanPidFile] });
before(() => setQuiescenceViewForTests(FILE_VIEW));
after(() => {
  restoreHermeticView();
  fs.rmSync(orphanPidDir, { recursive: true, force: true });
});

beforeEach(async () => {
  daemonDir = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-attempt-docker-"));
  daemon = await startFakeDaemon();
});

afterEach(async () => {
  for (const pid of orphans.splice(0)) {
    try {
      process.kill(pid, "SIGKILL");
    } catch {
      /* gone */
    }
  }
  await daemon.close();
  fs.rmSync(daemonDir, { recursive: true, force: true });
});

// ─── helpers ───────────────────────────────────────────────────────────────────────────────

/** The real quiescence primitive (process reaper + Docker teardown), with a fast Docker poll. */
const fastQuiesce = (req: QuiesceRunRequest): Promise<QuiesceRunOutcome> =>
  quiesceRunAttempt(req, { docker: { intervalMs: 5, deadlineMs: 3_000, requestTimeoutMs: 2_000 } });

/** A recorder around a quiescer: every request, in order. */
function recorded(inner: (req: QuiesceRunRequest) => Promise<QuiesceRunOutcome>) {
  const calls: QuiesceRunRequest[] = [];
  return {
    calls,
    quiesceRun: async (req: QuiesceRunRequest) => {
      calls.push(req);
      return inner(req);
    },
  };
}

/** Force `state` on every capture-mode proof; everything else goes to the real primitive. */
function captureAnswers(state: ProcessQuiescenceState) {
  return async (req: QuiesceRunRequest): Promise<QuiesceRunOutcome> => {
    if (req.mode !== "capture") return fastQuiesce(req);
    return {
      process: { state, processes: [], killed: [], detail: `scripted ${state}` },
      docker: { state: "docker_unconfirmed", removed: [], detail: "" },
    };
  };
}

function dockerHost(): string {
  return `unix://${daemon.socket}`;
}

function wired(factory: ExecutorFactory, extra: Parameters<typeof runnerWith>[4] = {}, log = nullLogger()) {
  const { gitlab } = fakeGitlab();
  return runnerWith(factory, gitlab, undefined, log, {
    dockerHost: dockerHost(),
    quiesceRun: fastQuiesce,
    recoveryRetryMs: 5,
    ...extra,
  });
}

/** A simulated worker restart: a NEW GitCache and RunRunner on the same dataDir, an EMPTY live
 *  set, the same (still running) Docker daemon. */
function restartedWorker(factory: ExecutorFactory, extra: Parameters<typeof runnerWith>[4] = {}, log = nullLogger()) {
  const { gitlab } = fakeGitlab();
  const restartedGit = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({ gitleaksBin: defaultGitleaksShim() }));
  const runner = new RunRunner(client, restartedGit, factory, log, 20, undefined, {
    pollMs: 5,
    planApprovalTimeoutMs: 0,
    questionTimeoutMs: 600,
    gitlab,
    dockerHost: dockerHost(),
    quiesceRun: fastQuiesce,
    liveAttempts: new LiveAttemptRegistry(),
    recoveryRetryMs: 5,
    ...extra,
  });
  return { runner, git: restartedGit };
}

function bare(): string {
  return git.barePathFor(fx.originPath);
}

function runnerRepoDir(): string {
  return path.join(fx.dataDir, "runner", path.basename(bare()).replace(/\.git$/, ""));
}

function canonicalFor(iid: number): string {
  return path.join(runnerRepoDir(), `issue-${iid}`);
}

function configGetAll(key: string): string[] {
  try {
    return execFileSync("git", ["-C", bare(), "config", "--local", "--get-all", key], { env: GIT_ENV, encoding: "utf8", stdio: "pipe" })
      .split("\n")
      .filter((l) => l !== "");
  } catch {
    return [];
  }
}

function readJournal(iid: number): { runId: string; clonePath: string; attemptId?: string } | undefined {
  const raw = configGetAll(`uzi-recovery.agent/issue-${iid}.clone`).at(-1);
  return raw ? (JSON.parse(raw) as { runId: string; clonePath: string; attemptId?: string }) : undefined;
}

type LedgerEntry = { attemptId: string; runId: string; clonePath: string; state: string };

/** The attempt ledger, the LAST value per attemptId winning. */
function readLedger(iid: number): Map<string, LedgerEntry> {
  const out = new Map<string, LedgerEntry>();
  for (const raw of configGetAll(`uzi-attempts.agent/issue-${iid}.entry`)) {
    const e = JSON.parse(raw) as LedgerEntry;
    out.set(e.attemptId, e);
  }
  return out;
}

/** An abandoned, unjournaled attempt dir of `issue-<iid>` with its ledger value (as a verified
 *  capture leaves it). */
async function plantAbandoned(iid: number, attemptId: string): Promise<string> {
  await git.ensureClone(fx.originPath);
  const dir = `${canonicalFor(iid)}.attempt-${attemptId}`;
  fs.mkdirSync(dir, { recursive: true });
  fs.writeFileSync(path.join(dir, "WORK.txt"), attemptId);
  execFileSync(
    "git",
    ["-C", bare(), "config", "--local", "--add", `uzi-attempts.agent/issue-${iid}.entry`, JSON.stringify({ attemptId, runId: randomUUID(), clonePath: dir, state: "abandoned" })],
    { env: GIT_ENV, stdio: "pipe" },
  );
  return dir;
}

/** Run `claim` to its end, bounded: past `ms` the run is cancelled server-side (its recovery loop
 *  then stops), so a regression that loops fails by assertion instead of the file's 120 s timeout.
 *  Returns true when the bound fired. */
async function boundedExecute(runner: RunRunner, claim: ReturnType<typeof gitlabClaim>, ms = 30_000): Promise<boolean> {
  let fired = false;
  const timer = setTimeout(() => {
    fired = true;
    api.setOwnershipStatus(claim.run_id, "cancelled");
  }, ms);
  try {
    await runner.execute(claim);
  } finally {
    clearTimeout(timer);
  }
  return fired;
}

function trackingHas(iid: number, file: string): boolean {
  try {
    execFileSync("git", ["-C", bare(), "cat-file", "-e", `refs/uzi-runner/agent/issue-${iid}:${file}`], { env: GIT_ENV, stdio: "pipe" });
    return true;
  } catch {
    return false;
  }
}

/** A content hash of a whole tree (paths + bytes + types), for an unchanged-tree assertion. */
function treeHash(root: string): string {
  const h = createHash("sha256");
  const walk = (dir: string): void => {
    for (const name of fs.readdirSync(dir).sort()) {
      const p = path.join(dir, name);
      const st = fs.lstatSync(p);
      h.update(`${path.relative(root, p)}\0${st.isDirectory() ? "d" : st.isSymbolicLink() ? "l" : "f"}\0`);
      if (st.isDirectory()) walk(p);
      else if (st.isFile()) h.update(fs.readFileSync(p));
    }
  };
  walk(root);
  return h.digest("hex");
}

/** AttemptSeedOptions for seeding a fixture directly through the git layer (no sweep). */
function fixtureSeed(attemptId: string): AttemptSeedOptions {
  return { attemptId, isLive: () => false, beforeSeed: async () => {}, quiescent: async () => true };
}

/** A predecessor attempt as a worker that died mid-run leaves it: an attempt clone with
 *  uncommitted work, its ledger `live` entry and its recovery journal naming it. */
async function seedPredecessor(iid: number, runId: string, opts: { attempt: boolean }) {
  const b = await git.ensureClone(fx.originPath);
  const attemptId = opts.attempt ? mintAttemptId(1) : undefined;
  const clone = await git.createOrAttachRunnerClone(b, iid, noProofReseed, runId, false, undefined, attemptId ? fixtureSeed(attemptId) : undefined);
  fs.writeFileSync(path.join(clone.path, "ONLY_COPY.txt"), "must survive recovery\n");
  await git.markRecoveryCapture(b, clone.path, `agent/issue-${iid}`, runId, clone.attemptId);
  return { clonePath: clone.path, attemptId };
}

/** An env-scrubbed process (no attempt marker, no worker mark) whose cwd is `cwd`, orphaned so
 *  it is NOT a descendant of this (the scanning) process: the double fork reparents it. */
function orphanIn(cwd: string, extraEnv: Record<string, string> = {}): number {
  const out = execFileSync("/bin/sh", ["-c", "sleep 300 </dev/null >/dev/null 2>&1 & echo $!"], {
    cwd,
    env: { PATH: process.env.PATH ?? "/usr/bin:/bin", ...extraEnv },
    encoding: "utf8",
  });
  const pid = Number(out.trim());
  orphans.push(pid);
  fs.appendFileSync(orphanPidFile, `${pid}\n`);
  return pid;
}

function alive(pid: number): boolean {
  try {
    process.kill(pid, 0);
    return true;
  } catch {
    return false;
  }
}

function transientFactory(onRun: (ctx: RunContext) => void = () => {}): { factory: ExecutorFactory; started: () => number } {
  let started = 0;
  const factory: ExecutorFactory = (runId) => ({
    homeDir: path.join(homeDir, runId),
    executor: {
      run: async (ctx: RunContext): Promise<ExecutorResult> => {
        started++;
        onRun(ctx);
        throw new Error("stop after the seed");
      },
    },
  });
  return { factory, started: () => started };
}

// ─── P-seed ────────────────────────────────────────────────────────────────────────────────

const LINUX_ONLY = process.platform !== "linux" ? "requires Linux runner process-proof and attributed-reap semantics" : false;

describe("issue #1783 M2 P-seed: a wired worker seeds <key>.attempt-<id>, an unwired one the canonical path", () => {
  it("wired: the clone, the marker, the journal and the ledger carry one attempt id", async () => {
    const iid = 2001;
    let seen: { worktree: string; marker?: string; journal?: ReturnType<typeof readJournal>; ledger?: Map<string, LedgerEntry> } | undefined;
    const { factory } = transientFactory((ctx) => {
      seen = { worktree: ctx.worktreePath, marker: ctx.runAttempt?.marker, journal: readJournal(iid), ledger: readLedger(iid) };
    });
    const claim = gitlabClaim(iid, { claim_generation: 7 });
    const { calls, quiesceRun } = recorded(fastQuiesce);
    await wired(factory, { quiesceRun }).execute(claim);
    assert.ok(seen, "the model started on a seeded clone");
    const parsed = parseAttemptPath(seen.worktree, path.join(fx.dataDir, "runner"));
    assert.ok(parsed, `the clone is an attempt path: ${seen.worktree}`);
    assert.equal(parsed.key, `issue-${iid}`);
    assert.match(parsed.attemptId, /-g7-/, "the claim generation is in the id");
    assert.equal(path.dirname(seen.worktree), runnerRepoDir());
    assert.equal(seen.marker, `${claim.run_id}:${parsed.attemptId}`, "the marker reuses the path's attempt id");
    assert.deepEqual(seen.journal, { runId: claim.run_id, clonePath: seen.worktree, attemptId: parsed.attemptId });
    assert.equal(seen.ledger?.get(parsed.attemptId)?.state, "live");
    assert.equal(fs.existsSync(canonicalFor(iid)), false, "the canonical path is never seeded");
    const own = calls.filter((c) => c.mode === "own");
    assert.ok(own.length > 0);
    for (const c of own) {
      assert.deepEqual(c.targetPaths, [seen.worktree], "an attempt's own footprint is its attempt path alone, never the canonical path");
    }
    // The failed run's terminal retire disposes of the attempt and marks it retired.
    assert.equal(readLedger(iid).get(parsed.attemptId)?.state, "retired");
    assert.equal(fs.existsSync(seen.worktree), false);
  });

  it("wired: a completing run finalizes from its attempt clone and retires it", async () => {
    const iid = 2004;
    simulateCommittedWork();
    let worktree = "";
    const factory: ExecutorFactory = (runId) => ({
      homeDir: path.join(homeDir, runId),
      executor: {
        run: async (ctx: RunContext): Promise<ExecutorResult> => {
          worktree = ctx.worktreePath;
          return { branch: ctx.branch, summary: "done" };
        },
      },
    });
    await wired(factory).execute(gitlabClaim(iid));
    assert.ok(api.states.some((s) => s.body.status === "completed"), "the run completes");
    const id = parseAttemptPath(worktree, path.join(fx.dataDir, "runner"))?.attemptId;
    assert.ok(id);
    assert.equal(readLedger(iid).get(id)?.state, "retired");
    assert.equal(fs.existsSync(worktree), false);
    assert.equal(readJournal(iid), undefined);
  });

  it("wired: a claim without a claim generation seeds a gx attempt", async () => {
    const iid = 2002;
    let worktree = "";
    const { factory } = transientFactory((ctx) => {
      worktree = ctx.worktreePath;
    });
    await wired(factory).execute(gitlabClaim(iid));
    assert.match(parseAttemptPath(worktree, path.join(fx.dataDir, "runner"))?.attemptId ?? "", /-gx-/);
  });

  it("unwired (control): the canonical path, a byte-identical journal, no ledger", async () => {
    const iid = 2003;
    let seen: { worktree: string; journalRaw?: string; ledger: string[] } | undefined;
    const factory: ExecutorFactory = (runId) => ({
      homeDir: path.join(homeDir, runId),
      executor: {
        run: async (ctx) => {
          seen = {
            worktree: ctx.worktreePath,
            journalRaw: configGetAll(`uzi-recovery.agent/issue-${iid}.clone`).at(-1),
            ledger: configGetAll(`uzi-attempts.agent/issue-${iid}.entry`),
          };
          throw new Error("stop");
        },
      },
    });
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(iid);
    await runnerWith(factory, gitlab).execute(claim);
    assert.equal(seen?.worktree, canonicalFor(iid));
    assert.equal(seen?.journalRaw, JSON.stringify({ runId: claim.run_id, clonePath: canonicalFor(iid) }));
    assert.deepEqual(seen?.ledger, []);
  });
});

// ─── P-resume-after-park + P-late-create ───────────────────────────────────────────────────

describe("issue #1783 M2 P-resume-after-park / P-late-create", { skip: !HAS_PROCFS }, () => {
  for (const restart of [false, true]) {
    it(`${restart ? "after a simulated worker restart" : "in-process"}: a verified park is retired, the resume seeds a fresh path, a late create lands only in the predecessor`, async () => {
      const iid = restart ? 2011 : 2012;
      const claim = gitlabClaim(iid, { wait_on_limit: true, claim_generation: 1 });
      let predecessor = "";
      let releaseCreate!: () => void;
      let lateCreate: Promise<string> | undefined;
      const parkFactory: ExecutorFactory = (runId) => ({
        homeDir: path.join(homeDir, runId),
        executor: {
          run: async (ctx: RunContext): Promise<ExecutorResult> => {
            predecessor = ctx.worktreePath;
            fs.mkdirSync(skillsPluginDir(ctx.worktreePath), { recursive: true });
            fs.writeFileSync(path.join(skillsPluginDir(ctx.worktreePath), "plugin.json"), "{}");
            fs.writeFileSync(path.join(ctx.worktreePath, "WORK.txt"), "committed work\n");
            execFileSync("git", ["-C", ctx.worktreePath, "add", "WORK.txt"], { env: GIT_ENV, stdio: "pipe" });
            execFileSync("git", ["-C", ctx.worktreePath, ...IDENT, "commit", "-m", "work"], { env: GIT_ENV, stdio: "pipe" });
            // A tool asks the daemon for a container binding this attempt's clone; the daemon
            // accepts it but completes it (and mkdirs the bind) only much later.
            daemon.hold = new Promise<void>((r) => {
              releaseCreate = r;
            });
            lateCreate = createContainer(daemon.socket, ctx.worktreePath);
            throw new LimitReachedError({ resetsAtMs: Date.now() + 5 * 3600_000, rateLimitType: "five_hour" });
          },
        },
      });
      const first = wired(parkFactory);
      await first.execute(claim);
      assert.ok(api.states.some((s) => s.body.status === "limit_wait"), "the run parked");
      const predId = parseAttemptPath(predecessor, path.join(fx.dataDir, "runner"))!.attemptId;
      assert.equal(readLedger(iid).get(predId)?.state, "retired", "the verified park retired the predecessor");
      assert.equal(fs.existsSync(predecessor), false, "the retired predecessor is gone from the runner dir");
      assert.ok(trackingHas(iid, "WORK.txt"), "the park fetched the work back into the tracking ref");
      assert.equal(fs.existsSync(skillsPluginDir(predecessor)), false, "a retired attempt's skills sibling goes with it, even on a park");

      // The resume (a resume carries the session id the run last reported).
      const { logger, lines } = recordingLogger();
      let observed:
        | { worktree: string; sessionId?: string | null; work: string; before: string; after: string; predecessorRecreated: boolean; predecessorEmpty: boolean }
        | undefined;
      const resumeFactory: ExecutorFactory = (runId) => ({
        homeDir: path.join(homeDir, runId),
        executor: {
          run: async (ctx: RunContext): Promise<ExecutorResult> => {
            const before = treeHash(ctx.worktreePath);
            releaseCreate();
            await lateCreate;
            observed = {
              worktree: ctx.worktreePath,
              sessionId: ctx.sessionId,
              work: fs.readFileSync(path.join(ctx.worktreePath, "WORK.txt"), "utf8"),
              before,
              after: treeHash(ctx.worktreePath),
              predecessorRecreated: fs.existsSync(predecessor),
              predecessorEmpty: fs.existsSync(predecessor) && fs.readdirSync(predecessor).length === 0,
            };
            throw new Error("stop after observing the resume");
          },
        },
      });
      const sessionId = randomUUID();
      const resumeClaim = { ...claim, session_id: sessionId, claim_generation: 2 };
      if (restart) {
        await restartedWorker(resumeFactory, {}, logger).runner.execute(resumeClaim);
      } else {
        const { gitlab } = fakeGitlab();
        await runnerWith(resumeFactory, gitlab, undefined, logger, {
          dockerHost: dockerHost(),
          quiesceRun: fastQuiesce,
          recoveryRetryMs: 5,
        }).execute(resumeClaim);
      }
      assert.ok(observed, "the resume started the model on a seeded clone");
      assert.notEqual(observed.worktree, predecessor, "the resume never reuses the predecessor path");
      assert.ok(parseAttemptPath(observed.worktree, path.join(fx.dataDir, "runner")), "the resume seeds a fresh attempt path");
      assert.match(observed.worktree, /-g2-[0-9a-f]{16}$/, "under the resume's claim generation");
      assert.equal(observed.work, "committed work\n", "the resume adopted the tracking ref");
      assert.equal(observed.sessionId, undefined, "the session is dropped: its cwd changed");
      assert.ok(
        lines.some((l) => (l as { reason?: string }).reason === "cwd_changed_attempt_path"),
        "the drop is logged with reason cwd_changed_attempt_path",
      );
      assert.equal(observed.after, observed.before, "the late create changed nothing in the successor's tree");
      assert.equal(observed.predecessorRecreated, true, "the late create's mkdir landed in the predecessor path");
      assert.equal(observed.predecessorEmpty, true, "and only as the bind's empty directory");
      const bound = [...daemon.containers.values()].flatMap((c) => c.Mounts.map((m) => m.Source));
      assert.deepEqual(bound, [predecessor], "the late container binds the predecessor, never the successor");
    });
  }
});

// ─── P-resume-unverified ───────────────────────────────────────────────────────────────────

describe("issue #1783 M2 P-resume-unverified (C′)", { skip: !HAS_PROCFS }, () => {
  it("captures the journaled predecessor behind a predecessor-scoped capture proof, releases it in place, then seeds fresh", async () => {
    const iid = 2021;
    const runId = randomUUID();
    const pred = await seedPredecessor(iid, runId, { attempt: true });
    const { calls, quiesceRun } = recorded(fastQuiesce);
    const { factory, started } = transientFactory();
    const claim = gitlabClaim(iid, { run_id: runId, session_id: randomUUID() });
    // A restarted worker (empty live set) meets the journal.
    const { runner } = restartedWorker(factory, { quiesceRun });
    await runner.execute(claim);

    assert.equal(started(), 0, "no model ever runs in (or for) the predecessor");
    assert.equal(calls[0]?.mode, "capture", "the first proof is a capture-mode sweep");
    assert.deepEqual(calls[0]?.targetPaths, [pred.clonePath], "scoped to the predecessor path alone");
    assert.equal(calls[0]?.attempt, undefined, "the predecessor is never quiesced as this flight's own attempt");
    assert.ok(calls.every((c) => c.mode === "capture" && c.targetPaths.length === 1 && c.targetPaths[0] === pred.clonePath),
      "every proof of the capture (its re-proofs and the pre-settle reap too) is predecessor-scoped");
    assert.equal(api.states.filter((s) => s.body.status === "recovery_wait").length, 1, "the capture verified and parked");
    assert.equal(fs.existsSync(path.join(pred.clonePath, "ONLY_COPY.txt")), true, "released IN PLACE: the path is still there");
    assert.equal(readJournal(iid), undefined, "the journal is cleared after the verified capture");
    assert.equal(readLedger(iid).get(pred.attemptId!)?.state, "abandoned");
    assert.ok(trackingHas(iid, "ONLY_COPY.txt"), "the captured work is in the tracking ref");

    // The requeued run seeds a FRESH path and gets the journaled work from the tracking ref.
    let fresh = "";
    let recovered = "";
    const next = transientFactory((ctx) => {
      fresh = ctx.worktreePath;
      recovered = fs.readFileSync(path.join(ctx.worktreePath, "ONLY_COPY.txt"), "utf8");
    });
    await restartedWorker(next.factory).runner.execute(claim);
    assert.equal(next.started(), 1);
    assert.notEqual(fresh, pred.clonePath);
    assert.ok(parseAttemptPath(fresh, path.join(fx.dataDir, "runner")));
    assert.equal(recovered, "must survive recovery\n");
    assert.equal(fs.existsSync(path.join(pred.clonePath, "ONLY_COPY.txt")), true, "the successor never touched the predecessor");
  });
});

describe("issue #1856: incomplete terminal disposal retains a predecessor journal", { skip: !HAS_PROCFS }, () => {
  it("does not release a verified predecessor in place after incomplete disposal", async () => {
    const iid = 18564;
    const runId = randomUUID();
    const pred = await seedPredecessor(iid, runId, { attempt: true });
    const before = readJournal(iid);
    const { factory: baseFactory, started } = codexFactory();
    const factory: ExecutorFactory = (id) => {
      const built = baseFactory(id);
      built.executor.safety!.dispose = async () => ({
        kind: "incomplete", errors: [{ category: "timeout", message: "predecessor drain unverified" }],
      });
      return built;
    };
    const { logger, lines } = recordingLogger();
    const quiesceRun = async (): Promise<QuiesceRunOutcome> => ({
      process: { state: "quiescent", processes: [], killed: [], detail: "verified" },
      docker: { state: "not_wired", removed: [], detail: "" },
    });
    const { runner } = restartedWorker(factory, { quiesceRun }, logger);
    await runner.execute(gitlabClaim(iid, { run_id: runId, session_id: randomUUID() }));
    assert.equal(started(), 0, "predecessor capture ends before a fresh model runs");
    assert.equal(trackingHas(iid, "ONLY_COPY.txt"), true, "the predecessor capture verified work before disposal");
    assert.ok(lines.some((line) => JSON.stringify(line).includes("predecessor drain unverified")), "the disposal reason is logged");
    assert.deepEqual(readJournal(iid), before, "incomplete disposal keeps the predecessor journal");
    assert.equal(readLedger(iid).get(pred.attemptId!)?.state, "live", "predecessor is not released");
    assert.equal(fs.existsSync(pred.clonePath), true);
  });
});

describe("issue #1783 M2 C′: the journal is cleared ONLY after a verified capture", { skip: !HAS_PROCFS }, () => {
  it("a run that turns terminal before its predecessor capture verified leaves journal, ledger and path as found", async () => {
    const iid = 2022;
    const runId = randomUUID();
    const pred = await seedPredecessor(iid, runId, { attempt: true });
    const journalBefore = readJournal(iid);
    api.setOwnershipStatus(runId, "cancelled");
    const { factory, started } = transientFactory();
    await wired(factory).execute(gitlabClaim(iid, { run_id: runId }));
    assert.equal(started(), 0);
    assert.deepEqual(readJournal(iid), journalBefore, "no verified capture, so the journal stays");
    assert.equal(readLedger(iid).get(pred.attemptId!)?.state, "live", "and nothing is released");
    assert.equal(fs.existsSync(path.join(pred.clonePath, "ONLY_COPY.txt")), true);
  });
});

describe("issue #1783 M2 × #1766: a vault-lock park's exit capture releases a predecessor in place", { skip: !HAS_PROCFS }, () => {
  it("a verified exit capture on a terminal ownership read marks the predecessor abandoned and clears the journal; the path is untouched", async () => {
    const iid = 2023;
    const runId = randomUUID();
    const pred = await seedPredecessor(iid, runId, { attempt: true });
    const { factory, started } = transientFactory();
    const runner = wired(factory);
    // Seam: the predecessor-capture flight parks through handleRecoveryExhausted with the transient
    // cause; drive that same park with the #1766 vault_locked cause, so the loop's terminal
    // ownership read exits through captureForVaultExit.
    type Park = (...a: unknown[]) => Promise<boolean>;
    const target = runner as unknown as { handleRecoveryExhausted: Park };
    const orig = target.handleRecoveryExhausted;
    const causes: string[] = [];
    target.handleRecoveryExhausted = function (this: unknown, ...a: unknown[]) {
      const flight = a[2] as { predecessorCapture: boolean };
      const cause = flight.predecessorCapture ? { kind: "vault_locked" } : a[7];
      causes.push((cause as { kind?: string } | undefined)?.kind ?? "transient");
      return orig.call(this, ...a.slice(0, 7), cause);
    };
    // The running confirmation is acked `running`; the loop's ownership read is terminal.
    api.setOwnershipStatus(runId, "cancelled");
    await runner.execute(gitlabClaim(iid, { run_id: runId }));

    assert.deepEqual(causes, ["vault_locked"], "the predecessor flight parked vault-locked, once");
    assert.equal(started(), 0, "no model ever runs in (or for) the predecessor");
    assert.equal(api.states.some((s) => s.body.status === "recovery_wait"), false, "a terminal run is never parked");
    assert.ok(trackingHas(iid, "ONLY_COPY.txt"), "the exit capture verified the predecessor's work into the tracking ref");
    assert.equal(readLedger(iid).get(pred.attemptId!)?.state, "abandoned", "released in place: the ledger says abandoned");
    assert.equal(readJournal(iid), undefined, "the journal is cleared after the verified exit capture");
    assert.equal(fs.readFileSync(path.join(pred.clonePath, "ONLY_COPY.txt"), "utf8"), "must survive recovery\n", "the path is untouched");
  });
});

// ─── P-capture-blocked ─────────────────────────────────────────────────────────────────────

describe("issue #1783 M2 P-capture-blocked", { skip: !HAS_PROCFS }, () => {
  it("survivors: an env-scrubbed process in the predecessor blocks the fetch-back; journal, ledger and path untouched", { skip: realTableSkip("P-capture-blocked: survivors") }, async () => {
    const iid = 2031;
    const runId = randomUUID();
    const pred = await seedPredecessor(iid, runId, { attempt: true });
    const journalBefore = readJournal(iid);
    const ledgerBefore = [...readLedger(iid).values()];
    const pid = orphanIn(pred.clonePath);
    const { factory, started } = transientFactory();
    const { calls, quiesceRun } = recorded(fastQuiesce);
    const timedOut = await boundedExecute(wired(factory, { quiesceRun }), gitlabClaim(iid, { run_id: runId }));
    assert.equal(timedOut, false, "the capture is refused up front, never retried to the bound");
    assert.equal(calls[0]?.site, "predecessor_capture", "the first proof is the up-front capture proof");
    const failed = api.states.filter((s) => s.body.status === "failed").at(-1)?.body;
    assert.equal(failed?.fail_origin, "worker_residue_blocked");
    assert.equal(started(), 0);
    assert.equal(alive(pid), true, "an unattributed in-scope process is a survivor, never killed");
    assert.deepEqual(readJournal(iid), journalBefore, "the journal is untouched");
    assert.deepEqual([...readLedger(iid).values()], ledgerBefore, "the ledger is untouched");
    assert.equal(fs.readFileSync(path.join(pred.clonePath, "ONLY_COPY.txt"), "utf8"), "must survive recovery\n");
    assert.equal(trackingHas(iid, "ONLY_COPY.txt"), false, "no fetch-back ran");
    assert.equal(api.states.some((s) => s.body.status === "recovery_wait"), false);
  });

  it("unverified: the same block", async () => {
    const iid = 2032;
    const runId = randomUUID();
    const pred = await seedPredecessor(iid, runId, { attempt: true });
    const journalBefore = readJournal(iid);
    const ledgerBefore = [...readLedger(iid).values()];
    const { factory, started } = transientFactory();
    const { calls, quiesceRun } = recorded(captureAnswers("unverified"));
    const timedOut = await boundedExecute(wired(factory, { quiesceRun }), gitlabClaim(iid, { run_id: runId }));
    assert.equal(timedOut, false, "the capture is refused up front, never retried to the bound");
    assert.equal(calls[0]?.site, "predecessor_capture");
    assert.equal(api.states.filter((s) => s.body.status === "failed").at(-1)?.body.fail_origin, "worker_residue_blocked");
    assert.equal(started(), 0);
    assert.deepEqual(readJournal(iid), journalBefore);
    assert.deepEqual([...readLedger(iid).values()], ledgerBefore);
    assert.equal(fs.existsSync(path.join(pred.clonePath, "ONLY_COPY.txt")), true);
    assert.equal(trackingHas(iid, "ONLY_COPY.txt"), false);
  });
});

// ─── the real primitive over a fake proc root: an unreadable, unattributed process ──────────
//
// The CI incident's shape (a same-uid non-dumpable process anywhere on the worker, e.g. a setgid
// ssh-agent), planted in a FAKE proc root so the outcome never depends on the host. The real
// quiescence primitive (no scripted quiescer) must fail closed at the capture and the terminal
// retire; the fresh attempt seed alone is allowed through (#1783 N6: it moves nothing).

describe("issue #1783 hermetic: an unreadable_unattributed process on a fake proc root (wired)", () => {
  /** Run `fn` with a fake proc root holding one unreadable, unattributed same-uid process. */
  function withUnreadable<T>(fn: () => Promise<T>): Promise<T> {
    const root = makeFakeProcRoot();
    plantUnreadableUnattributed(root, 4242);
    return withQuiescenceView({ procRoot: root }, fn, FILE_VIEW).finally(() => fs.rmSync(root, { recursive: true, force: true }));
  }

  it("capture: the predecessor capture is refused (worker_residue_blocked); journal, ledger and path untouched", { skip: LINUX_ONLY }, async () => {
    const iid = 2041;
    const runId = randomUUID();
    const pred = await seedPredecessor(iid, runId, { attempt: true });
    const journalBefore = readJournal(iid);
    const ledgerBefore = [...readLedger(iid).values()];
    const { factory, started } = transientFactory();
    const { calls, quiesceRun } = recorded(fastQuiesce);
    const timedOut = await withUnreadable(() => boundedExecute(wired(factory, { quiesceRun }), gitlabClaim(iid, { run_id: runId })));
    assert.equal(timedOut, false);
    assert.equal(calls[0]?.site, "predecessor_capture");
    assert.equal(api.states.filter((s) => s.body.status === "failed").at(-1)?.body.fail_origin, "worker_residue_blocked");
    assert.equal(started(), 0, "no model started");
    assert.deepEqual(readJournal(iid), journalBefore);
    assert.deepEqual([...readLedger(iid).values()], ledgerBefore);
    assert.equal(fs.existsSync(path.join(pred.clonePath, "ONLY_COPY.txt")), true);
    assert.equal(trackingHas(iid, "ONLY_COPY.txt"), false, "no fetch-back ran");
  });

  it("control: the same capture over an EMPTY fake root verifies, fetches back and parks", async () => {
    const iid = 2042;
    const runId = randomUUID();
    await seedPredecessor(iid, runId, { attempt: true });
    const { factory, started } = transientFactory();
    const root = makeFakeProcRoot();
    const timedOut = await withQuiescenceView({ procRoot: root }, () => boundedExecute(wired(factory), gitlabClaim(iid, { run_id: runId })), FILE_VIEW);
    fs.rmSync(root, { recursive: true, force: true });
    assert.equal(timedOut, false);
    assert.equal(started(), 0);
    assert.equal(api.states.filter((s) => s.body.status === "recovery_wait").length, 1, "the capture verified and parked");
    assert.equal(readJournal(iid), undefined, "the journal is cleared after the verified capture");
    assert.equal(trackingHas(iid, "ONLY_COPY.txt"), true, "the predecessor's work was fetched back");
  });

  it("seed + terminal retire: the fresh attempt seeds (N6), but the retire keeps the clone and the ledger entry live", { skip: LINUX_ONLY }, async () => {
    const iid = 2043;
    let worktree = "";
    const { factory, started } = transientFactory((ctx) => {
      worktree = ctx.worktreePath;
    });
    const { calls, quiesceRun } = recorded(fastQuiesce);
    await withUnreadable(() => wired(factory, { quiesceRun }).execute(gitlabClaim(iid)));
    assert.equal(started(), 1, "the fresh attempt path was seeded despite the unverified sweep");
    assert.ok(calls.some((c) => c.site === "attempt_seed"));
    const id = parseAttemptPath(worktree, path.join(fx.dataDir, "runner"))?.attemptId;
    assert.ok(id);
    assert.notEqual(readLedger(iid).get(id)?.state, "retired", "the terminal retire failed closed");
    assert.equal(fs.existsSync(worktree), true, "the clone is kept");
  });
});

// ─── P-worker-restart (B′) + P-foreign (runner level) ──────────────────────────────────────

function failureOf(runId: string): string {
  const body = api.states.filter((s) => s.runId === runId && s.body.status === "failed").at(-1)?.body;
  return String(body?.failure_reason ?? "");
}

function terminalOwner(ownerRunId: string, iid: number): void {
  api.setOrphanClassification(ownerRunId, {
    status: "completed",
    repo_id: "r1",
    kind: "issue",
    issue_iid: iid,
    branch: null,
    pipeline_ref: null,
    pipeline_id: null,
  });
}

describe("issue #1783 M2 P-worker-restart / P-foreign: the terminal-orphan reclaim (d′)", { skip: !HAS_PROCFS }, () => {
  it("B′ after a restart: a ledger-recorded owner attempt is released IN PLACE and the claimant seeds fresh", async () => {
    const iid = 2041;
    const owner = randomUUID();
    const pred = await seedPredecessor(iid, owner, { attempt: true });
    terminalOwner(owner, iid);
    let fresh = "";
    const { factory } = transientFactory((ctx) => {
      fresh = ctx.worktreePath;
    });
    const claimant = randomUUID();
    await restartedWorker(factory).runner.execute(gitlabClaim(iid, { run_id: claimant }));
    assert.ok(fresh && fresh !== pred.clonePath, "the claimant runs at its own fresh attempt path");
    assert.equal(fs.readFileSync(path.join(pred.clonePath, "ONLY_COPY.txt"), "utf8"), "must survive recovery\n", "foreign work retained in place, not moved");
    assert.equal(readLedger(iid).get(pred.attemptId!)?.state, "reclaimed", "uncaptured: reclaimed, never abandoned");
    assert.equal(readJournal(iid)?.runId === owner, false, "the owner's journal is released");
  });

  it("B′ regression (review probe): a reclaimed foreign attempt survives later seeds past the abandoned cap", async () => {
    const iid = 2044;
    const owner = randomUUID();
    const pred = await seedPredecessor(iid, owner, { attempt: true });
    terminalOwner(owner, iid);
    await restartedWorker(transientFactory().factory).runner.execute(gitlabClaim(iid));
    assert.equal(readLedger(iid).get(pred.attemptId!)?.state, "reclaimed");
    // Three NEWER abandoned attempts of the same key, then another seed: were the reclaimed
    // attempt counted as abandoned it would be the oldest of four and deleted (the probe printed
    // OWNER_DIR_EXISTS=false STATE=retired).
    for (let i = 1; i <= 3; i++) await plantAbandoned(iid, formatAttemptId(new Date(Date.now() + i * 60_000), 1, `00000000000000${i}${i}`));
    const next = transientFactory();
    await wired(next.factory).execute(gitlabClaim(iid));
    assert.equal(next.started(), 1, "the later seed ran");
    assert.equal(fs.existsSync(pred.clonePath), true, "OWNER_DIR_EXISTS");
    assert.equal(fs.readFileSync(path.join(pred.clonePath, "ONLY_COPY.txt"), "utf8"), "must survive recovery\n");
    assert.equal(readLedger(iid).get(pred.attemptId!)?.state, "reclaimed", "STATE stays reclaimed");
  });

  it("orphan_reclaim: a survivor in the owner's attempt path blocks the reclaim; nothing released (worker_residue_blocked)", { skip: realTableSkip("P-worker-restart / P-foreign: orphan_reclaim survivor") }, async () => {
    const iid = 2045;
    const owner = randomUUID();
    const pred = await seedPredecessor(iid, owner, { attempt: true });
    terminalOwner(owner, iid);
    const journalBefore = readJournal(iid);
    const pid = orphanIn(pred.clonePath);
    const { calls, quiesceRun } = recorded(fastQuiesce);
    const { factory, started } = transientFactory();
    const claim = gitlabClaim(iid);
    await wired(factory, { quiesceRun }).execute(claim);
    assert.equal(started(), 0);
    assert.ok(calls.some((c) => c.site === "orphan_reclaim" && c.mode === "capture" && c.targetPaths.join() === pred.clonePath), "the predecessor-scoped proof ran");
    assert.equal(api.states.filter((s) => s.runId === claim.run_id && s.body.status === "failed").at(-1)?.body.fail_origin, "worker_residue_blocked");
    assert.equal(alive(pid), true, "never killed on attribution-by-cwd alone");
    assert.deepEqual(readJournal(iid), journalBefore, "the owner's journal is kept");
    assert.equal(readLedger(iid).get(pred.attemptId!)?.state, "live", "no release was recorded");
    assert.equal(fs.readFileSync(path.join(pred.clonePath, "ONLY_COPY.txt"), "utf8"), "must survive recovery\n");
  });

  it("rejects an id the ledger does not record for the owner: fail closed, the path untouched", async () => {
    const iid = 2042;
    const owner = randomUUID();
    const b = await git.ensureClone(fx.originPath);
    const unrecorded = formatAttemptId(new Date(), 3, "0123456789abcdef");
    const planted = `${canonicalFor(iid)}.attempt-${unrecorded}`;
    fs.mkdirSync(planted, { recursive: true });
    fs.writeFileSync(path.join(planted, "FOREIGN.txt"), "x\n");
    await git.markRecoveryCapture(b, planted, `agent/issue-${iid}`, owner, unrecorded);
    terminalOwner(owner, iid);
    const { factory, started } = transientFactory();
    const claim = gitlabClaim(iid);
    await wired(factory).execute(claim);
    assert.equal(started(), 0);
    assert.match(failureOf(claim.run_id), /refusing to replace a retained clone owned by another run/, "fails closed on (d′)");
    assert.equal(fs.readFileSync(path.join(planted, "FOREIGN.txt"), "utf8"), "x\n");
    assert.equal(readJournal(iid)?.clonePath, planted, "the journal is untouched");
    assert.equal(readLedger(iid).size, 0);
  });

  it("rejects issue-17.attempt-… journaled for key issue-1769, and a traversal: fail closed, the paths untouched", async () => {
    const b = await git.ensureClone(fx.originPath);
    const owner = randomUUID();
    const id = formatAttemptId(new Date(), 1, "fedcba9876543210");
    const other = path.join(runnerRepoDir(), `issue-17.attempt-${id}`);
    fs.mkdirSync(other, { recursive: true });
    fs.writeFileSync(path.join(other, "FOREIGN.txt"), "x\n");
    await git.markRecoveryCapture(b, other, "agent/issue-1769", owner, id);
    terminalOwner(owner, 1769);
    const { factory, started } = transientFactory();
    const c1769 = gitlabClaim(1769);
    await wired(factory).execute(c1769);
    assert.equal(started(), 0);
    assert.match(failureOf(c1769.run_id), /recovery journal points at a different clone path/);
    assert.equal(fs.existsSync(path.join(other, "FOREIGN.txt")), true);
    assert.equal(readJournal(1769)?.clonePath, other);

    const iid = 2043;
    const tid = formatAttemptId(new Date(), 1, "00000000000000aa");
    const real = `${canonicalFor(iid)}.attempt-${tid}`;
    fs.mkdirSync(real, { recursive: true });
    fs.writeFileSync(path.join(real, "FOREIGN.txt"), "x\n");
    const traversal = path.join(runnerRepoDir(), "..", path.basename(runnerRepoDir()), `issue-${iid}.attempt-${tid}`).replace(
      `${path.sep}${path.basename(runnerRepoDir())}${path.sep}`,
      `${path.sep}${path.basename(runnerRepoDir())}${path.sep}..${path.sep}${path.basename(runnerRepoDir())}${path.sep}`,
    );
    assert.ok(traversal.includes(`${path.sep}..${path.sep}`), `a traversal path: ${traversal}`);
    await git.markRecoveryCapture(b, traversal, `agent/issue-${iid}`, owner, tid);
    terminalOwner(owner, iid);
    const ct = gitlabClaim(iid);
    await wired(factory).execute(ct);
    assert.equal(started(), 0);
    assert.match(failureOf(ct.run_id), /recovery journal points at a different clone path/);
    assert.equal(fs.existsSync(path.join(real, "FOREIGN.txt")), true);
    assert.equal(readJournal(iid)?.clonePath, traversal);
  });
});

// ─── P-legacy ──────────────────────────────────────────────────────────────────────────────

describe("issue #1783 M2 P-legacy", { skip: !HAS_PROCFS }, () => {
  it("a canonical-path journal on a newly wired worker is captured, released in place and never reused", async () => {
    const iid = 2051;
    const runId = randomUUID();
    const legacy = await seedPredecessor(iid, runId, { attempt: false });
    assert.equal(legacy.clonePath, canonicalFor(iid));
    assert.equal(readJournal(iid)?.attemptId, undefined, "an older worker's journal carries no attempt id");
    const { factory, started } = transientFactory();
    const claim = gitlabClaim(iid, { run_id: runId });
    await wired(factory).execute(claim);
    assert.equal(started(), 0);
    assert.equal(api.states.filter((s) => s.body.status === "recovery_wait").length, 1);
    assert.equal(fs.existsSync(path.join(legacy.clonePath, "ONLY_COPY.txt")), true, "released in place");
    assert.equal(readJournal(iid), undefined);
    assert.equal(readLedger(iid).size, 0, "a canonical path has no attempt identity to ledger");

    let fresh = "";
    const next = transientFactory((ctx) => {
      fresh = ctx.worktreePath;
    });
    await wired(next.factory).execute(claim);
    assert.ok(parseAttemptPath(fresh, path.join(fx.dataDir, "runner")), "the canonical path is never reused");
    assert.equal(fs.existsSync(path.join(legacy.clonePath, "ONLY_COPY.txt")), true);
  });
});

// ─── P-resume-check ────────────────────────────────────────────────────────────────────────

describe("issue #1783 M2 P-resume-check", () => {
  function plantTranscript(runId: string, sessionId: string): void {
    const dir = path.join(homeDir, runId, ".claude", "projects", "-some-old-cwd");
    fs.mkdirSync(dir, { recursive: true });
    fs.writeFileSync(path.join(dir, `${sessionId}.jsonl`), "{}\n");
  }

  it("wired: never reports a resumable session, even with the old transcript under HOME (Claude and Codex)", async () => {
    const realInspect = CodexSessionStore.inspectSession;
    CodexSessionStore.inspectSession = async () => "present";
    try {
      for (const codex of [false, true]) {
        const iid = codex ? 2061 : 2062;
        const sessionId = randomUUID();
        const claim = gitlabClaim(iid, {
          session_id: sessionId,
          ...(codex
            ? { secrets: { forge_pat: "fixture-forge-pat-000000", codex: { auth_mode: "api_key" as const, access_token: "codex-at-fixture", capability: "codex-cap-fixture" } } }
            : {}),
        });
        plantTranscript(claim.run_id, sessionId);
        let seen: string | null | undefined = "unset";
        const { factory } = transientFactory((ctx) => {
          seen = ctx.sessionId;
        });
        await wired(factory).execute(claim);
        assert.equal(seen, undefined, `${codex ? "Codex" : "Claude"}: a fresh session`);
        assert.equal(
          api.messages(claim.run_id).some((m) => (m.payload as { event?: string })?.event === "resume_continued"),
          false,
          "no resumable-session signal",
        );
      }
    } finally {
      CodexSessionStore.inspectSession = realInspect;
    }
  });

  it("unwired (control): the same transcript resumes", async () => {
    const iid = 2063;
    const sessionId = randomUUID();
    const claim = gitlabClaim(iid, { session_id: sessionId });
    plantTranscript(claim.run_id, sessionId);
    let seen: string | null | undefined;
    const { factory } = transientFactory((ctx) => {
      seen = ctx.sessionId;
    });
    const { gitlab } = fakeGitlab();
    await runnerWith(factory, gitlab).execute(claim);
    assert.equal(seen, sessionId);
  });
});

// ─── seed-time sweep ───────────────────────────────────────────────────────────────────────

describe("issue #1783 M2: the seed-time sweep over the key's NON-LIVE paths", { skip: !HAS_PROCFS }, () => {
  /** An abandoned, unjournaled predecessor attempt (released in place earlier). */
  async function abandonedPredecessor(iid: number): Promise<string> {
    const runId = randomUUID();
    const pred = await seedPredecessor(iid, runId, { attempt: true });
    await git.releaseAttemptInPlace(bare(), pred.clonePath, `agent/issue-${iid}`, runId, "abandoned");
    return pred.clonePath;
  }

  function attemptDirs(iid: number): string[] {
    return fs.readdirSync(runnerRepoDir()).filter((n) => n.startsWith(`issue-${iid}.attempt-`));
  }

  it("survivors: an unattributed process in a non-live path blocks the seed; nothing is seeded or moved", { skip: realTableSkip("seed-time sweep: survivors") }, async () => {
    const iid = 2071;
    const pred = await abandonedPredecessor(iid);
    const pid = orphanIn(pred);
    const { factory, started } = transientFactory();
    const claim = gitlabClaim(iid);
    await wired(factory).execute(claim);
    assert.equal(started(), 0);
    assert.equal(api.states.filter((s) => s.body.status === "failed").at(-1)?.body.fail_origin, "worker_residue_blocked");
    assert.equal(alive(pid), true);
    assert.deepEqual(attemptDirs(iid), [path.basename(pred)], "no new attempt path was seeded");
    assert.equal(fs.existsSync(path.join(pred, "ONLY_COPY.txt")), true, "the predecessor is untouched");
    assert.equal([...readLedger(iid).values()].some((e) => e.runId === claim.run_id), false, "no ledger entry for the blocked seed");
  });

  it("a live-owner conflict (a live same-key attempt's process) blocks the seed", { skip: realTableSkip("seed-time sweep: live-owner conflict") }, async () => {
    const iid = 2072;
    const liveAttempts = new LiveAttemptRegistry();
    const livePath = `${canonicalFor(iid)}.attempt-${mintAttemptId(1)}`;
    fs.mkdirSync(livePath, { recursive: true });
    const other = newRunAttempt(randomUUID(), 1, livePath, () => []);
    liveAttempts.add(other);
    const pid = orphanIn(livePath, { [RUN_ATTEMPT_ENV]: other.marker, [RUN_CLONE_KEY_ENV]: other.cloneKey });
    const { factory, started } = transientFactory();
    await wired(factory, { liveAttempts }).execute(gitlabClaim(iid));
    assert.equal(started(), 0);
    assert.equal(api.states.filter((s) => s.body.status === "failed").at(-1)?.body.fail_origin, "worker_residue_blocked");
    assert.equal(alive(pid), true, "a live attempt's process is never signalled");
  });

  it("unverified blocks the seed too", async () => {
    const iid = 2073;
    await abandonedPredecessor(iid);
    const { factory, started } = transientFactory();
    const scripted = async (req: QuiesceRunRequest): Promise<QuiesceRunOutcome> =>
      req.mode === "seed"
        ? { process: { state: "unverified", processes: [], killed: [], detail: "scripted" }, docker: { state: "not_wired", removed: [], detail: "" } }
        : fastQuiesce(req);
    await wired(factory, { quiesceRun: scripted }).execute(gitlabClaim(iid));
    assert.equal(started(), 0);
    assert.equal(api.states.filter((s) => s.body.status === "failed").at(-1)?.body.fail_origin, "worker_residue_blocked");
    assert.equal(attemptDirs(iid).length, 1);
  });

  it("kills a terminal attempt's residue, removes containers bound in non-live paths, keeps a live same-key attempt's and a sibling key's", { skip: realTableSkip("seed-time sweep: kills a terminal attempt's residue") }, async () => {
    const iid = 2074;
    const pred = await abandonedPredecessor(iid);
    const deadMarker = `${randomUUID()}:${mintAttemptId(1)}`;
    const cloneKey = `${path.basename(runnerRepoDir())}/issue-${iid}`;
    const residuePid = orphanIn(pred, { [RUN_ATTEMPT_ENV]: deadMarker, [RUN_CLONE_KEY_ENV]: cloneKey });
    const liveAttempts = new LiveAttemptRegistry();
    const livePath = `${canonicalFor(iid)}.attempt-${mintAttemptId(1)}`;
    fs.mkdirSync(livePath, { recursive: true });
    liveAttempts.add(newRunAttempt(randomUUID(), 1, livePath, () => []));
    const sibling = path.join(runnerRepoDir(), `issue-${iid}0`);
    const predContainer = await createContainer(daemon.socket, path.join(pred, "data"));
    const liveContainer = await createContainer(daemon.socket, path.join(livePath, "data"));
    const siblingContainer = await createContainer(daemon.socket, sibling);
    let fresh = "";
    const { factory } = transientFactory((ctx) => {
      fresh = ctx.worktreePath;
    });
    await wired(factory, { liveAttempts }).execute(gitlabClaim(iid));
    assert.ok(fresh, "the seed proceeded");
    for (let i = 0; i < 100 && alive(residuePid); i++) await new Promise((r) => setTimeout(r, 20));
    assert.equal(alive(residuePid), false, "a terminal attempt's marked residue is killed");
    assert.equal(daemon.containers.has(predContainer), false, "the non-live path's container is removed");
    assert.equal(daemon.containers.has(liveContainer), true, "a live same-key attempt's container is kept");
    assert.equal(daemon.containers.has(siblingContainer), true, "a sibling key's container is kept");
  });
});

// ─── review round: Codex sweeps, the release warning, seed availability, checkpoint adoption ──

/** A Codex-shaped executor (it carries `safety`) that stops after the seed. */
function codexFactory(onRun: (ctx: RunContext) => void = () => {}): { factory: ExecutorFactory; started: () => number } {
  let started = 0;
  const safety: CodexExecutionSafety = {
    kind: "codex",
    withBoundary: async (req, action) => action({ epoch: 1, boundary: req.boundary, signal: new AbortController().signal } as unknown as BoundaryPermit),
    spawnBoundaryProcess: async (_permit, request) => {
      const [command, ...args] = request.argv;
      const child = spawn(command!, args, { cwd: request.cwd, env: request.env, stdio: ["pipe", "pipe", "pipe"] });
      const completed = new Promise<{ code: number }>((resolve, reject) => {
        child.once("error", reject);
        child.once("exit", (code, sig) => resolve({ code: code ?? (sig ? 128 : 1) }));
      });
      return { stdin: child.stdin, stdout: child.stdout, stderr: child.stderr, cancel: async () => { if (child.exitCode === null && child.signalCode === null) child.kill("SIGKILL"); await completed; }, completed };
    },
    dispose: async () => ({ kind: "disposed" }),
  };
  const factory: ExecutorFactory = (runId) => ({
    homeDir: path.join(homeDir, runId),
    executor: {
      safety,
      run: async (ctx: RunContext): Promise<ExecutorResult> => {
        started++;
        onRun(ctx);
        throw new Error("stop after the seed");
      },
    },
  });
  return { factory, started: () => started };
}

describe("issue #1783 M2 review: a Codex run's seed and capture sweeps scan processes", { skip: !HAS_PROCFS }, () => {
  it("seed: a survivor in a non-live path blocks a Codex run's seed exactly as a Claude run's", { skip: realTableSkip("Codex sweeps: seed survivor") }, async () => {
    const iid = 2081;
    const runId = randomUUID();
    const pred = await seedPredecessor(iid, runId, { attempt: true });
    await git.releaseAttemptInPlace(bare(), pred.clonePath, `agent/issue-${iid}`, runId, "abandoned");
    const pid = orphanIn(pred.clonePath);
    const { calls, quiesceRun } = recorded(fastQuiesce);
    const { factory, started } = codexFactory();
    await wired(factory, { quiesceRun }).execute(gitlabClaim(iid));
    const seed = calls.filter((c) => c.mode === "seed");
    assert.ok(seed.length > 0 && seed.every((c) => c.processes === true), "every seed sweep scans processes");
    assert.equal(started(), 0, "the seed is blocked");
    assert.equal(api.states.filter((s) => s.body.status === "failed").at(-1)?.body.fail_origin, "worker_residue_blocked");
    assert.equal(alive(pid), true);
  });

  it("capture: a survivor in the journaled predecessor blocks a Codex run's capture", { skip: realTableSkip("Codex sweeps: capture survivor") }, async () => {
    const iid = 2082;
    const runId = randomUUID();
    const pred = await seedPredecessor(iid, runId, { attempt: true });
    const journalBefore = readJournal(iid);
    orphanIn(pred.clonePath);
    const { calls, quiesceRun } = recorded(fastQuiesce);
    const { factory, started } = codexFactory();
    const timedOut = await boundedExecute(wired(factory, { quiesceRun }), gitlabClaim(iid, { run_id: runId }));
    assert.equal(timedOut, false);
    const capture = calls.filter((c) => c.mode === "capture");
    assert.ok(capture.length > 0 && capture.every((c) => c.processes === true), "every capture sweep scans processes");
    assert.equal(started(), 0);
    assert.equal(api.states.filter((s) => s.body.status === "failed").at(-1)?.body.fail_origin, "worker_residue_blocked");
    assert.deepEqual(readJournal(iid), journalBefore);
    assert.equal(trackingHas(iid, "ONLY_COPY.txt"), false, "no fetch-back ran");
  });
});

describe("issue #1783 M2 review: the predecessor release warning names what actually happened", { skip: !HAS_PROCFS }, () => {
  it("a failed ledger append keeps the journal, and the warning says so", async () => {
    const iid = 2091;
    const runId = randomUUID();
    const pred = await seedPredecessor(iid, runId, { attempt: true });
    const { logger, lines } = recordingLogger();
    const { runner, git: rg } = restartedWorker(transientFactory().factory, {}, logger);
    const seam = rg as unknown as { runGit: (cwd: string | undefined, args: string[], ...rest: unknown[]) => Promise<string> };
    const realRunGit = seam.runGit.bind(rg);
    seam.runGit = async (cwd, args, ...rest) => {
      if (args[0] === "config" && args.includes("--add") && args.some((a) => a.startsWith("uzi-attempts.")) && args.some((a) => a.includes('"abandoned"'))) {
        throw new Error("injected ledger append failure");
      }
      return realRunGit(cwd, args, ...rest);
    };
    await runner.execute(gitlabClaim(iid, { run_id: runId, session_id: randomUUID() }));
    assert.ok(trackingHas(iid, "ONLY_COPY.txt"), "the capture itself verified");
    const warn = lines.find((l) => ((l as { msg?: string }).msg ?? "").startsWith("predecessor attempt release"));
    assert.equal((warn as { msg?: string } | undefined)?.msg, "predecessor attempt release failed at the ledger append; journal kept");
    assert.equal(readJournal(iid)?.clonePath, pred.clonePath, "the journal IS kept, as the warning says");
    assert.equal(readLedger(iid).get(pred.attemptId!)?.state, "live", "no release recorded");
  });

  // NB5: the "refused" message is keyed on the mismatch class; every other pre-write failure (the
  // bare-lock wait, the journal read, its parse) gets an accurate generic message.
  for (const kind of ["mismatch", "generic"] as const) {
    it(`a ${kind === "mismatch" ? "journal mismatch" : "pre-write (lock / config read / parse)"} failure is named as such`, async () => {
      const iid = kind === "mismatch" ? 2092 : 2093;
      const runId = randomUUID();
      const pred = await seedPredecessor(iid, runId, { attempt: true });
      const { logger, lines } = recordingLogger();
      const { runner, git: rg } = restartedWorker(transientFactory().factory, {}, logger);
      const seam = rg as unknown as { releaseAttemptInPlace: (...a: unknown[]) => Promise<void> };
      seam.releaseAttemptInPlace = async () => {
        throw kind === "mismatch"
          ? new CapturePathMismatchError("", pred.clonePath, `agent/issue-${iid}`, runId)
          : new Error("injected: could not read the bare config");
      };
      await runner.execute(gitlabClaim(iid, { run_id: runId, session_id: randomUUID() }));
      const warn = lines.find((l) => ((l as { msg?: string }).msg ?? "").startsWith("predecessor attempt release"));
      assert.equal(
        (warn as { msg?: string } | undefined)?.msg,
        kind === "mismatch"
          ? "predecessor attempt release refused (the journal no longer names this attempt); nothing released"
          : "predecessor attempt release failed before any write (bare-lock wait, journal read or parse); nothing released, journal kept as found",
      );
      assert.equal(readJournal(iid)?.clonePath, pred.clonePath, "the journal is kept");
    });
  }
});

describe("issue #1783 M2 review (N6): seed availability under an unattributable unreadable process", { skip: !HAS_PROCFS }, () => {
  const unreadable = { pid: 4242, uid: 10002, comm: "nondumpable", cwd: "unreadable", reason: "unreadable_unattributed" };

  /** Scripted proofs: `site` answers `unverified` with `processes`; every other proof is quiescent. */
  function scriptedAt(site: string, processes: Array<typeof unreadable>) {
    return async (req: QuiesceRunRequest): Promise<QuiesceRunOutcome> =>
      req.site === site
        ? { process: { state: "unverified", processes, killed: [], detail: "scripted" }, docker: { state: "not_wired", removed: [], detail: "" } }
        : { process: { state: "quiescent", processes: [], killed: [], detail: "scripted" }, docker: { state: "not_wired", removed: [], detail: "" } };
  }

  it("seed: seeds the fresh path, logs, and vetoes every retention deletion of that seed", async () => {
    const iid = 2101;
    const planted: string[] = [];
    for (const i of [1, 2, 3, 4, 5]) planted.push(await plantAbandoned(iid, formatAttemptId(new Date(Date.UTC(2026, 0, 1, 0, 0, i)), 1, `000000000000000${i}`)));
    const { logger, lines } = recordingLogger();
    const { factory, started } = transientFactory();
    await wired(factory, { quiesceRun: scriptedAt("attempt_seed", [unreadable]) }, logger).execute(gitlabClaim(iid));
    assert.equal(started(), 1, "the fresh seed is not blocked");
    assert.ok(lines.some((l) => ((l as { msg?: string }).msg ?? "").includes("unattributable unreadable")), "logged");
    for (const p of planted) assert.equal(fs.existsSync(p), true, `retention deletion vetoed: ${p}`);
  });

  it("seed: a positively in-scope survivor alongside it still blocks", async () => {
    const iid = 2102;
    await plantAbandoned(iid, formatAttemptId(new Date(Date.UTC(2026, 0, 1)), 1, "0000000000000001"));
    const { factory, started } = transientFactory();
    await wired(factory, { quiesceRun: scriptedAt("attempt_seed", [unreadable, { ...unreadable, pid: 7, reason: "unattributed_in_scope" }]) }).execute(gitlabClaim(iid));
    assert.equal(started(), 0);
    assert.equal(api.states.filter((s) => s.body.status === "failed").at(-1)?.body.fail_origin, "worker_residue_blocked");
  });

  it("capture: the same unattributable unreadable process still fails closed", async () => {
    const iid = 2103;
    const runId = randomUUID();
    const pred = await seedPredecessor(iid, runId, { attempt: true });
    const { factory, started } = transientFactory();
    const timedOut = await boundedExecute(wired(factory, { quiesceRun: scriptedAt("predecessor_capture", [unreadable]) }), gitlabClaim(iid, { run_id: runId }));
    assert.equal(timedOut, false);
    assert.equal(started(), 0);
    assert.equal(api.states.filter((s) => s.body.status === "failed").at(-1)?.body.fail_origin, "worker_residue_blocked");
    assert.equal(readJournal(iid)?.clonePath, pred.clonePath);
    assert.equal(trackingHas(iid, "ONLY_COPY.txt"), false);
  });
});

describe("issue #1783 M2 review: checkpoint adoption on a wired resume that seeds a fresh attempt path", () => {
  /** Publish a checkpoint commit (main + M1.txt) at origin's refs/uzi-checkpoints/agent/issue-<iid>. */
  function publishOriginCheckpoint(iid: number): string {
    const o = fx.originPath;
    const run = (args: string[], env: NodeJS.ProcessEnv = GIT_ENV, input?: string) =>
      execFileSync("git", ["-C", o, ...args], { env, encoding: "utf8", stdio: "pipe", input }).trim();
    const blob = run(["hash-object", "-w", "--stdin"], GIT_ENV, "milestone one\n");
    const index = path.join(daemonDir, `ckpt-index-${iid}`);
    const ienv = { ...GIT_ENV, GIT_INDEX_FILE: index };
    run(["read-tree", "main"], ienv);
    run(["update-index", "--add", "--cacheinfo", `100644,${blob},M1.txt`], ienv);
    const tree = run(["write-tree"], ienv);
    const tip = run([...IDENT, "commit-tree", tree, "-p", "main", "-m", "M1"]);
    run(["update-ref", `refs/uzi-checkpoints/agent/issue-${iid}`, tip]);
    return tip;
  }

  for (const own of [true, false]) {
    it(`${own ? "adopts" : "(control) does not adopt"} the run's own checkpoint ${own ? "when checkpoint_tip matches" : "without checkpoint_tip"}`, async () => {
      const iid = own ? 2111 : 2112;
      const tip = publishOriginCheckpoint(iid);
      // A predecessor attempt of the same key is retained, so the resume must seed a FRESH path.
      const pred = await plantAbandoned(iid, formatAttemptId(new Date(Date.UTC(2026, 0, 1)), 1, "00000000000000aa"));
      let seen: { worktree: string; head: string; m1: boolean } | undefined;
      const { factory } = transientFactory((ctx) => {
        seen = {
          worktree: ctx.worktreePath,
          head: execFileSync("git", ["-C", ctx.worktreePath, "rev-parse", "HEAD"], { env: GIT_ENV, encoding: "utf8" }).trim(),
          m1: fs.existsSync(path.join(ctx.worktreePath, "M1.txt")),
        };
      });
      const claim = gitlabClaim(iid, { session_id: randomUUID(), claim_generation: 3, ...(own ? { checkpoint_tip: tip } : {}) });
      await restartedWorker(factory).runner.execute(claim);
      assert.ok(seen, "the model started");
      assert.notEqual(seen.worktree, pred);
      assert.match(parseAttemptPath(seen.worktree, path.join(fx.dataDir, "runner"))?.attemptId ?? "", /-g3-/, "a fresh attempt path");
      assert.equal(seen.m1, own, own ? "the checkpoint's milestone is in the fresh attempt" : "a foreign/unowned checkpoint is not adopted");
      if (own) assert.equal(seen.head, tip, "seeded at the checkpoint tip");
    });
  }
});

describe("issue #1783: a planted fresh-attempt path fails typed worker_residue_blocked", () => {
  it("an existing <key>.attempt-<id> path refuses the seed, touches nothing, and classifies as worker_residue_blocked", async () => {
    const iid = 91;
    const b = await git.ensureClone(fx.originPath);
    const attemptId = mintAttemptId(1);
    const planted = `${canonicalFor(iid)}.attempt-${attemptId}`;
    fs.mkdirSync(planted, { recursive: true });
    fs.writeFileSync(path.join(planted, "PLANTED.txt"), "not ours\n");
    const before = treeHash(planted);
    const err = await git
      .createOrAttachRunnerClone(b, iid, noProofReseed, randomUUID(), false, undefined, fixtureSeed(attemptId))
      .then(() => undefined, (e: unknown) => e as Error);
    assert.ok(err instanceof Error, "the seed is refused");
    assert.equal(failOriginForReason(err.message), "worker_residue_blocked", `classified from: ${err.message}`);
    assert.equal(treeHash(planted), before, "the planted path is left exactly as found");
  });
});
