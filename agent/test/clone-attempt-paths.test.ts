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
import { CapturePathMismatchError, GitCache, type AttemptSeedOptions, type RecoveryJournalEntry } from "../src/git.js";
import { LimitReachedError } from "../src/limit.js";
import { RunRunner, failOriginForReason, type ExecutorFactory } from "../src/runner.js";
import { CodexSessionStore } from "../src/codex/session-state.js";
import { formatAttemptId, parseAttemptPath } from "../src/attempt-path.js";
import { InvalidRecoveryClonePathError } from "../src/recovery-progress.js";
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
import { latchResidueQuarantine } from "../src/residue-quarantine.js";
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
  plantAfterFetch,
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

function readJournal(iid: number): RecoveryJournalEntry | undefined {
  const raw = configGetAll(`uzi-recovery.agent/issue-${iid}.clone`).at(-1);
  return raw ? (JSON.parse(raw) as RecoveryJournalEntry) : undefined;
}

function assertChargedSource(iid: number, source: RecoveryJournalEntry, blocker?: string): void {
  const journal = readJournal(iid);
  assert.ok(journal?.recovery);
  assert.equal(journal.runId, source.runId);
  assert.equal(journal.clonePath, source.clonePath);
  assert.equal(journal.attemptId, source.attemptId);
  assert.deepEqual(journal.recovery.source, source);
  assert.equal(journal.recovery.version, 1);
  assert.equal(journal.recovery.attempts, 1, "reservation is charged before ownership/proof");
  assert.equal(journal.recovery.deadline - journal.recovery.startedAt, 300_000);
  assert.equal(journal.recovery.stage, blocker ? "blocked" : "capturing");
  assert.equal(journal.recovery.blocker, blocker);
}

function assertRetainedSuccess(iid: number, runId: string, pred: { clonePath: string; attemptId?: string }, fresh: string): void {
  const journal = readJournal(iid);
  assert.ok(journal?.recovery);
  assert.equal(journal.clonePath, fresh);
  assert.equal(journal.runId, runId);
  assert.equal(journal.recovery.attempts, 1);
  assert.equal(journal.recovery.stage, "ready-for-model");
  assert.deepEqual(journal.recovery.source, { runId, clonePath: pred.clonePath, ...(pred.attemptId ? { attemptId: pred.attemptId } : {}) });
  assert.deepEqual(journal.recovery.successor, { runId, clonePath: fresh, attemptId: journal.attemptId, restoreTip: journal.recovery.restoreTip });
  assert.ok(journal.retainedSources?.some(s => s.clonePath === pred.clonePath && s.runId === runId && s.attemptId === pred.attemptId));
  assert.equal(fs.readFileSync(path.join(pred.clonePath, "ONLY_COPY.txt"), "utf8"), "must survive recovery\n");
  if (pred.attemptId) assert.equal(readLedger(iid).get(pred.attemptId)?.state, "live");
  assert.ok(trackingHas(iid, "ONLY_COPY.txt"));
  assert.match(journal.recovery.restoreTip ?? "", /^[0-9a-f]{40}$/);
  assert.equal(execFileSync("git", ["-C", bare(), "rev-parse", `refs/uzi-recovery-episode/${runId}/${journal.recovery.restoreTip}`], { env: GIT_ENV, encoding: "utf8" }).trim(), journal.recovery.restoreTip);
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
  api.setOwnershipStatus(runId, "running", 2);
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
  for (const unwired of [false, true]) {
    it(`verified retained capture seeds fresh within the claim (${unwired ? "unwired" : "wired"})`, async () => {
      const iid = unwired ? 2024 : 2021;
      const runId = randomUUID();
      const pred = await seedPredecessor(iid, runId, { attempt: true });
      const { calls, quiesceRun } = recorded(fastQuiesce);
      let fresh = "";
      let session: string | null | undefined;
      let recovered = "";
      let admittedJournal: RecoveryJournalEntry | undefined;
      const { factory, started } = transientFactory((ctx) => {
        fresh = ctx.worktreePath;
        session = ctx.sessionId;
        recovered = fs.readFileSync(path.join(fresh, "ONLY_COPY.txt"), "utf8");
        admittedJournal = readJournal(iid);
      });
      const claim = gitlabClaim(iid, { run_id: runId, claim_generation: 2, session_id: randomUUID() });
      const { runner } = restartedWorker(factory, { quiesceRun, ...(unwired ? { dockerHost: undefined } : {}) });
      await runner.execute(claim);
      assert.equal(started(), 1, "the same claim admits a fresh model");
      assert.notEqual(fresh, pred.clonePath);
      assert.equal(session, undefined, "fresh cwd drops the predecessor session");
      assert.equal(recovered, "must survive recovery\n");
      assert.equal(admittedJournal?.recovery?.stage, "ready-for-model");
      assert.equal(calls[0]?.mode, "capture");
      const captures = calls.filter(c => c.mode === "capture");
      assert.ok(captures.length > 0);
      for (const c of captures) {
        assert.deepEqual(c.targetPaths, [pred.clonePath]);
        assert.equal(c.attempt, undefined);
      }
      assert.match(fresh, /-g2-[0-9a-f]{16}$/);
      assert.equal(api.states.some(s => s.body.status === "recovery_wait"), false);
      assertRetainedSuccess(iid, runId, pred, fresh);
    });
  }
});

describe("issue #1856 / #2512: unproven settlement retains a charged predecessor journal", { skip: !HAS_PROCFS }, () => {
  for (const unsupported of [false, true]) {
    it(`${unsupported ? "unsupported" : "incomplete"} settlement blocks capture and model admission`, async () => {
      const iid = 18564;
      const runId = randomUUID();
      const pred = await seedPredecessor(iid, runId, { attempt: true });
      const before = readJournal(iid);
      const { factory: baseFactory, started } = codexFactory();
      const factory: ExecutorFactory = (id) => {
        const built = baseFactory(id);
        if (unsupported) delete built.executor.settleForCredentialFreeCapture;
        else built.executor.settleForCredentialFreeCapture = async () => ({
          kind: "incomplete", errors: [{ category: "timeout", message: "predecessor drain unverified" }],
        });
        return built;
      };
      const { logger } = recordingLogger();
      const quiesceRun = async (): Promise<QuiesceRunOutcome> => ({
        process: { state: "quiescent", processes: [], killed: [], detail: "verified" },
        docker: { state: "not_wired", removed: [], detail: "" },
      });
      const { runner } = restartedWorker(factory, { quiesceRun }, logger);
      await runner.execute(gitlabClaim(iid, { run_id: runId, claim_generation: 2, session_id: randomUUID() }));
      assert.equal(started(), 0, "predecessor capture ends before a fresh model runs");
      assert.equal(trackingHas(iid, "ONLY_COPY.txt"), false, "unproven settlement blocks before capture");
      assert.equal(api.states.some(s => s.body.status === "recovery_wait"), false);
      assertChargedSource(iid, before!, "quiescence_failed");
      assert.equal(api.states.filter(s => s.body.status === "failed").at(-1)?.body.fail_origin, "worker_residue_blocked");
      assert.equal(readLedger(iid).get(pred.attemptId!)?.state, "live", "predecessor is not released");
      assert.equal(fs.existsSync(pred.clonePath), true);
    });
  }
});

describe("issue #2512: terminal ownership retains the charged source", { skip: !HAS_PROCFS }, () => {
  it("terminal ownership stops before capture while retaining identity, ledger and path", async () => {
    const iid = 2022;
    const runId = randomUUID();
    const pred = await seedPredecessor(iid, runId, { attempt: true });
    const journalBefore = readJournal(iid);
    api.setOwnershipStatus(runId, "cancelled");
    const { factory, started } = transientFactory();
    await wired(factory).execute(gitlabClaim(iid, { run_id: runId, claim_generation: 2 }));
    assert.equal(started(), 0);
    assertChargedSource(iid, journalBefore!);
    assert.equal(readLedger(iid).get(pred.attemptId!)?.state, "live", "and nothing is released");
    assert.equal(fs.existsSync(path.join(pred.clonePath, "ONLY_COPY.txt")), true);
  });
});

describe("issue #2512 × #1766: dead ownership never enters the automatic vault park", { skip: !HAS_PROCFS }, () => {
  it("terminal ownership never invokes the vault park or predecessor release", async () => {
    const iid = 2023;
    const runId = randomUUID();
    const pred = await seedPredecessor(iid, runId, { attempt: true });
    const { factory, started } = transientFactory();
    const runner = wired(factory);
    let releases = 0;
    git.releaseAttemptInPlace = async () => {
      releases++;
      throw new Error("dead ownership must not release the predecessor");
    };
    // Trap the former automatic vault-park hook: retained recovery must never enter it.
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
    await runner.execute(gitlabClaim(iid, { run_id: runId, claim_generation: 2 }));

    assert.deepEqual(causes, [], "dead ownership never invokes the automatic park hook");
    assert.equal(releases, 0, "dead ownership never invokes predecessor release");
    assert.equal(started(), 0, "no model ever runs in (or for) the predecessor");
    assert.equal(api.states.some((s) => s.body.status === "recovery_wait"), false, "a terminal run is never parked");
    assert.equal(trackingHas(iid, "ONLY_COPY.txt"), false, "dead ownership stops before capture mutation");
    assert.equal(readLedger(iid).get(pred.attemptId!)?.state, "live", "dead ownership retains the source ledger");
    assertChargedSource(iid, { runId, ...pred });
    assert.equal(fs.readFileSync(path.join(pred.clonePath, "ONLY_COPY.txt"), "utf8"), "must survive recovery\n", "the path is untouched");
  });
});

describe("issue #2512: exact ownership and durable recovery budget", () => {
  for (const generation of [undefined, 3]) {
    it(`ownership generation ${generation ?? "missing"} stops before proof or mutation`, async () => {
      const iid = generation === undefined ? 2025 : 2026;
      const runId = randomUUID();
      const pred = await seedPredecessor(iid, runId, { attempt: true });
      const before = treeHash(pred.clonePath);
      const source = readJournal(iid)!;
      api.setOwnershipStatus(runId, "running", generation);
      const { factory, started } = transientFactory();
      const { calls, quiesceRun } = recorded(fastQuiesce);
      await restartedWorker(factory, { quiesceRun }).runner.execute(gitlabClaim(iid, { run_id: runId, claim_generation: 2 }));
      assert.equal(started(), 0);
      assert.deepEqual(calls, [], "no quiescence or mutation authority on a stale claim");
      assertChargedSource(iid, source);
      assert.equal(treeHash(pred.clonePath), before);
      assert.equal(readLedger(iid).get(pred.attemptId!)?.state, "live");
      assert.equal(trackingHas(iid, "ONLY_COPY.txt"), false);
      assert.equal(api.states.some(s => s.body.status === "recovery_wait" || s.body.status === "failed"), false);
    });
  }

  it("three unverified captures are precharged and exhaustion retains the source without parking", async () => {
    const iid = 2027;
    const runId = randomUUID();
    const pred = await seedPredecessor(iid, runId, { attempt: true });
    const before = treeHash(pred.clonePath);
    const { factory, started } = transientFactory();
    const { runner, git: rg } = restartedWorker(factory);
    const charges: number[] = [];
    rg.worktreeStatus = async () => {
      charges.push(readJournal(iid)?.recovery?.attempts ?? 0);
      return null;
    };
    assert.equal(await boundedExecute(runner, gitlabClaim(iid, { run_id: runId, claim_generation: 2 })), false);
    assert.deepEqual(charges, [1, 2, 3], "each operation sees its persisted reservation");
    assert.equal(started(), 0);
    const journal = readJournal(iid);
    assert.equal(journal?.recovery?.attempts, 3);
    assert.equal(journal?.recovery?.stage, "blocked");
    assert.equal(journal?.recovery?.blocker, "budget_exhausted");
    assert.deepEqual(journal?.recovery?.source, { runId, ...pred });
    assert.equal(journal?.clonePath, pred.clonePath);
    assert.equal(treeHash(pred.clonePath), before);
    assert.equal(readLedger(iid).get(pred.attemptId!)?.state, "live");
    assert.equal(api.states.some(s => s.body.status === "recovery_wait"), false);
    assert.match(failureOf(runId), /Retained recovery blocked: budget exhausted.*local work and custody retained/);
    assert.notEqual(api.states.filter(s => s.body.status === "failed").at(-1)?.body.fail_origin, "worker_residue_blocked");
  });
});

// ─── P-capture-blocked ─────────────────────────────────────────────────────────────────────

describe("issue #1783 M2 P-capture-blocked", { skip: !HAS_PROCFS }, () => {
  it("survivors: an env-scrubbed process in the predecessor blocks the fetch-back; charged journal with ledger and path retained", { skip: realTableSkip("P-capture-blocked: survivors") }, async () => {
    const iid = 2031;
    const runId = randomUUID();
    const pred = await seedPredecessor(iid, runId, { attempt: true });
    const journalBefore = readJournal(iid);
    const ledgerBefore = [...readLedger(iid).values()];
    const pid = orphanIn(pred.clonePath);
    const { factory, started } = transientFactory();
    const { calls, quiesceRun } = recorded(fastQuiesce);
    const timedOut = await boundedExecute(wired(factory, { quiesceRun }), gitlabClaim(iid, { run_id: runId, claim_generation: 2 }));
    assert.equal(timedOut, false, "the capture is refused up front, never retried to the bound");
    assert.equal(calls[0]?.site, "predecessor_capture", "the first proof is the up-front capture proof");
    const failed = api.states.filter((s) => s.body.status === "failed").at(-1)?.body;
    assert.equal(failed?.fail_origin, "worker_residue_blocked");
    assert.equal(started(), 0);
    assert.equal(alive(pid), true, "an unattributed in-scope process is a survivor, never killed");
    assertChargedSource(iid, journalBefore!, "quiescence_failed");
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
    const timedOut = await boundedExecute(wired(factory, { quiesceRun }), gitlabClaim(iid, { run_id: runId, claim_generation: 2 }));
    assert.equal(timedOut, false, "the capture is refused up front, never retried to the bound");
    assert.equal(calls[0]?.site, "predecessor_capture");
    assert.equal(api.states.filter((s) => s.body.status === "failed").at(-1)?.body.fail_origin, "worker_residue_blocked");
    assert.equal(started(), 0);
    assertChargedSource(iid, journalBefore!, "quiescence_failed");
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
    // issue #2213: planted right after the run's clone fetch: the worker-wide pre-fetch check would
    // otherwise refuse the run before any of the later proof sites this suite exercises.
    plantAfterFetch(() => plantUnreadableUnattributed(root, 4242));
    return withQuiescenceView({ procRoot: root }, fn, FILE_VIEW).finally(() => fs.rmSync(root, { recursive: true, force: true }));
  }

  it("capture: the predecessor capture is refused (worker_residue_blocked); charged journal with ledger and path retained", { skip: LINUX_ONLY }, async () => {
    const iid = 2041;
    const runId = randomUUID();
    const pred = await seedPredecessor(iid, runId, { attempt: true });
    const journalBefore = readJournal(iid);
    const ledgerBefore = [...readLedger(iid).values()];
    const { factory, started } = transientFactory();
    const { calls, quiesceRun } = recorded(fastQuiesce);
    const timedOut = await withUnreadable(() => boundedExecute(wired(factory, { quiesceRun }), gitlabClaim(iid, { run_id: runId, claim_generation: 2 })));
    assert.equal(timedOut, false);
    assert.equal(calls[0]?.site, "predecessor_capture");
    assert.equal(api.states.filter((s) => s.body.status === "failed").at(-1)?.body.fail_origin, "worker_residue_blocked");
    assert.equal(started(), 0, "no model started");
    assertChargedSource(iid, journalBefore!, "quiescence_failed");
    assert.deepEqual([...readLedger(iid).values()], ledgerBefore);
    assert.equal(fs.existsSync(path.join(pred.clonePath, "ONLY_COPY.txt")), true);
    assert.equal(trackingHas(iid, "ONLY_COPY.txt"), false, "no fetch-back ran");
  });

  it("control: an EMPTY fake root verifies capture and admits a fresh successor", async () => {
    const iid = 2042;
    const runId = randomUUID();
    const pred = await seedPredecessor(iid, runId, { attempt: true });
    let fresh = "";
    const { factory, started } = transientFactory(ctx => { fresh = ctx.worktreePath; });
    const root = makeFakeProcRoot();
    const timedOut = await withQuiescenceView({ procRoot: root }, () => boundedExecute(wired(factory), gitlabClaim(iid, { run_id: runId, claim_generation: 2 })), FILE_VIEW);
    fs.rmSync(root, { recursive: true, force: true });
    assert.equal(timedOut, false);
    assert.equal(started(), 1);
    assert.equal(api.states.some(s => s.body.status === "recovery_wait"), false);
    assertRetainedSuccess(iid, runId, pred, fresh);
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
    assert.notEqual(api.states.filter(s => s.runId === c1769.run_id && s.body.status === "failed").at(-1)?.body.fail_origin, "worker_residue_blocked", "path mismatch is not a quiescence failure");
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
    await assert.rejects(git.markRecoveryCapture(b, traversal, `agent/issue-${iid}`, owner, tid), InvalidRecoveryClonePathError);
    execFileSync("git", ["-C", b, "config", "--local", `uzi-recovery.agent/issue-${iid}.clone`,
      JSON.stringify({ runId: owner, clonePath: traversal, attemptId: tid })], { env: GIT_ENV, stdio: "pipe" });
    terminalOwner(owner, iid);
    const ct = gitlabClaim(iid, { claim_generation: 2 });
    api.setOwnershipStatus(ct.run_id, "running", 2);
    await wired(factory).execute(ct);
    assert.equal(started(), 0);
    assert.match(failureOf(ct.run_id), /Retained recovery blocked: corrupt journal or unsafe local recovery storage/);
    assert.notEqual(api.states.filter(s => s.runId === ct.run_id && s.body.status === "failed").at(-1)?.body.fail_origin, "worker_residue_blocked", "corrupt identity is not a quiescence failure");
    assert.equal(fs.existsSync(path.join(real, "FOREIGN.txt")), true);
    assert.equal(readJournal(iid)?.clonePath, traversal);
  });
});

// ─── P-legacy ──────────────────────────────────────────────────────────────────────────────

describe("issue #1783 M2 P-legacy", { skip: !HAS_PROCFS }, () => {
  it("a canonical predecessor retains its descriptor and seeds a fresh successor", async () => {
    const iid = 2051;
    const runId = randomUUID();
    const legacy = await seedPredecessor(iid, runId, { attempt: false });
    assert.equal(legacy.clonePath, canonicalFor(iid));
    assert.equal(readJournal(iid)?.attemptId, undefined);
    let fresh = "";
    const { factory, started } = transientFactory(ctx => { fresh = ctx.worktreePath; });
    await wired(factory).execute(gitlabClaim(iid, { run_id: runId, claim_generation: 2 }));
    assert.equal(started(), 1);
    assert.ok(parseAttemptPath(fresh, path.join(fx.dataDir, "runner")));
    assert.notEqual(fresh, legacy.clonePath);
    assertRetainedSuccess(iid, runId, legacy, fresh);
    assert.equal(readJournal(iid)?.recovery?.source.attemptId, undefined, "no fabricated canonical attempt identity");
    assert.equal(readLedger(iid).size, 1, "only the fresh successor has a ledger identity");
    assert.equal(api.states.some(s => s.body.status === "recovery_wait"), false);
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
      settleForCredentialFreeCapture: async () => ({ kind: "observed_empty" as const }),
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
    const pid = orphanIn(pred.clonePath);
    const ledgerBefore = [...readLedger(iid).values()];
    const { calls, quiesceRun } = recorded(fastQuiesce);
    const { factory, started } = codexFactory();
    const timedOut = await boundedExecute(wired(factory, { quiesceRun }), gitlabClaim(iid, { run_id: runId, claim_generation: 2 }));
    assert.equal(timedOut, false);
    const capture = calls.filter((c) => c.mode === "capture");
    assert.ok(capture.length > 0 && capture.every((c) => c.processes === true), "every capture sweep scans processes");
    assert.equal(alive(pid), true);
    assert.deepEqual([...readLedger(iid).values()], ledgerBefore);
    assert.equal(fs.readFileSync(path.join(pred.clonePath, "ONLY_COPY.txt"), "utf8"), "must survive recovery\n");
    assert.equal(started(), 0);
    assert.equal(api.states.filter((s) => s.body.status === "failed").at(-1)?.body.fail_origin, "worker_residue_blocked");
    assertChargedSource(iid, journalBefore!, "quiescence_failed");
    assert.equal(trackingHas(iid, "ONLY_COPY.txt"), false, "no fetch-back ran");
  });
});

describe("issue #2512: protected recovery never invokes predecessor release", { skip: !HAS_PROCFS }, () => {
  for (const kind of ["append", "mismatch", "config", "latched"] as const) {
    it(`retains source and journal despite an injected ${kind} release fault`, async () => {
      const iid = { append: 2091, mismatch: 2092, config: 2093, latched: 2094 }[kind];
      const runId = randomUUID();
      const pred = await seedPredecessor(iid, runId, { attempt: true });
      let fresh = "";
      const { factory, started } = transientFactory(ctx => {
        fresh = ctx.worktreePath;
        if (kind === "latched") latchResidueQuarantine({ cause: "c", runId, site: "terminal_drive" }, nullLogger());
      });
      const { logger, lines } = recordingLogger();
      const { runner, git: rg } = restartedWorker(factory, {}, logger);
      let releases = 0;
      rg.releaseAttemptInPlace = async () => {
        releases++;
        throw kind === "mismatch"
          ? new CapturePathMismatchError("", pred.clonePath, `agent/issue-${iid}`, runId)
          : new Error(`injected ${kind} release failure`);
      };
      const seam = rg as unknown as { runGit: (cwd: string | undefined, args: string[], ...rest: unknown[]) => Promise<string> };
      const real = seam.runGit.bind(rg);
      let abandonedAppends = 0;
      seam.runGit = async (cwd, args, ...rest) => {
        if (args[0] === "config" && args.includes("--add") && args.some(a => a.startsWith("uzi-attempts.")) && args.some(a => a.includes('"abandoned"'))) {
          abandonedAppends++;
          throw new Error("injected ledger append failure");
        }
        return real(cwd, args, ...rest);
      };
      await runner.execute(gitlabClaim(iid, { run_id: runId, claim_generation: 2, session_id: randomUUID() }));
      assert.equal(started(), 1, "protected capture admits the fresh successor");
      assert.equal(releases, 0, "finally never calls the release helper");
      assert.equal(abandonedAppends, 0, "no abandoned ledger append attempted");
      assert.equal(lines.some(l => ((l as { msg?: string }).msg ?? "").startsWith("predecessor attempt release")), false);
      assertRetainedSuccess(iid, runId, pred, fresh);
      assert.equal(api.states.some(s => s.body.status === "recovery_wait"), false);
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
    const timedOut = await boundedExecute(wired(factory, { quiesceRun: scriptedAt("predecessor_capture", [unreadable]) }), gitlabClaim(iid, { run_id: runId, claim_generation: 2 }));
    assert.equal(timedOut, false);
    assert.equal(started(), 0);
    assert.equal(api.states.filter((s) => s.body.status === "failed").at(-1)?.body.fail_origin, "worker_residue_blocked");
    assertChargedSource(iid, { runId, ...pred }, "quiescence_failed");
    assert.equal(readLedger(iid).get(pred.attemptId!)?.state, "live");
    assert.equal(fs.readFileSync(path.join(pred.clonePath, "ONLY_COPY.txt"), "utf8"), "must survive recovery\n");
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


describe("missing-source restarted worker ordering", () => {
  const command = (...args: string[]) => execFileSync("git", ["-C", bare(), ...args], { env: GIT_ENV, encoding: "utf8" }).trim();
  const pins = () => command("for-each-ref", "--format=%(refname) %(objectname)", "refs/uzi");

  async function missingPrimary(iid: number, owner: string, attempt: boolean, episode: boolean) {
    const pred = await seedPredecessor(iid, owner, { attempt });
    if (episode) await git.reserveRecoveryIteration(bare(), `agent/issue-${iid}`, `issue-${iid}`, { runId: owner, ...pred }, 5);
    const siblingId = mintAttemptId(2);
    const sibling = `${canonicalFor(iid)}.attempt-${siblingId}`;
    fs.cpSync(pred.clonePath, sibling, { recursive: true });
    command("config", "--add", `uzi-attempts.agent/issue-${iid}.entry`,
      JSON.stringify({ attemptId: siblingId, runId: owner, clonePath: sibling, state: "live" }));
    const journal = { ...readJournal(iid)!, retainedSources: [{ runId: owner, clonePath: sibling, attemptId: siblingId }] };
    command("config", `uzi-recovery.agent/issue-${iid}.clone`, JSON.stringify(journal));
    const tip = command("rev-parse", "refs/remotes/origin/main");
    for (const family of ["uzi-recovery-episode", "uzi-owed"]) command("update-ref", `refs/${family}/${owner}/${tip}`, tip);
    fs.rmSync(pred.clonePath, { recursive: true });
    return { pred, sibling, journal, hash: treeHash(sibling), pinSnapshot: pins() };
  }

  it("missing-source ledger-recorded foreign attempt after restart reclaim", async t => {
    const iid = 25130, owner = randomUUID(), claimant = randomUUID();
    const pred = await seedPredecessor(iid, owner, { attempt: true });
    fs.rmSync(pred.clonePath, { recursive: true });
    terminalOwner(owner, iid);
    const { logger, lines } = recordingLogger();
    let entryChecked = false;
    const { factory, started } = transientFactory(ctx => {
      assert.notEqual(ctx.worktreePath, pred.clonePath);
      assert.ok(parseAttemptPath(ctx.worktreePath, path.join(fx.dataDir, "runner")));
      assert.equal(fs.existsSync(path.join(ctx.worktreePath, "ONLY_COPY.txt")), false);
      assert.equal(fs.readFileSync(path.join(ctx.worktreePath, "README.md"), "utf8"), "# fixture\n");
      assert.equal(readJournal(iid)?.runId, claimant);
      assert.equal(readJournal(iid)?.clonePath, ctx.worktreePath);
      entryChecked = true;
    });
    const worker = restartedWorker(factory, {}, logger);
    const release = worker.git.releaseAttemptInPlace.bind(worker.git);
    let reclaimed = false;
    t.mock.method(worker.git, "releaseAttemptInPlace", async (...args: Parameters<typeof release>) => {
      const result = await release(...args);
      if (args[1] === pred.clonePath && args[3] === owner && args[4] === "reclaimed") {
        assert.equal(readLedger(iid).get(pred.attemptId!)?.state, "reclaimed");
        reclaimed = true;
      }
      return result;
    });
    await worker.runner.execute(gitlabClaim(iid, { run_id: claimant }));
    assert.equal(started(), 1);
    assert.equal(entryChecked, true, "all fresh claimant entry assertions ran");
    assert.equal(reclaimed, true, "observe release before seed compacts missing ledger entries");
    assert.ok(lines.some(l => (l as { msg?: string; owner_id?: string }).msg === "orphan_reclaim_succeeded"
      && (l as { owner_id?: string }).owner_id === owner));
  });

  for (const attempt of [false, true]) {
    for (const episode of [false, true]) {
      it(`missing-source terminal ${attempt ? "attempt" : "canonical"} primary with surviving sibling ${episode ? "episode" : "legacy"} reclaim`, async t => {
        const iid = 25131, owner = randomUUID(), claimant = randomUUID();
        const s = await missingPrimary(iid, owner, attempt, episode);
        terminalOwner(owner, iid);
        const { logger, lines } = recordingLogger();
        let releases = 0;
        for (const method of ["releaseRecoveryCustody", "settleRecoveryHold", "settleRecoveryHoldLive"] as const) {
          t.mock.method(client, method, async () => { releases++; throw new Error("unexpected custody release"); });
        }
        let entryChecked = false;
        const { factory, started } = transientFactory(ctx => {
          assert.notEqual(ctx.worktreePath, s.pred.clonePath);
          assert.notEqual(ctx.worktreePath, s.sibling);
          assert.ok(parseAttemptPath(ctx.worktreePath, path.join(fx.dataDir, "runner")));
          assert.equal(fs.existsSync(path.join(ctx.worktreePath, "ONLY_COPY.txt")), false);
          assert.equal(readJournal(iid)?.runId, claimant);
          assert.equal(readJournal(iid)?.clonePath, ctx.worktreePath);
          assert.deepEqual(JSON.parse(configGetAll(`uzi-retained.${owner}.journal`)[0]!), {
            version: 1, branch: `agent/issue-${iid}`, key: `issue-${iid}`, journal: s.journal,
          });
          entryChecked = true;
        });
        const worker = restartedWorker(factory, { dockerHost: undefined }, logger);
        assert.equal(await boundedExecute(worker.runner, gitlabClaim(iid, { run_id: claimant })), false);
        assert.equal(started(), 1);
        assert.equal(entryChecked, true, "all fresh claimant entry assertions ran");
        assert.equal(releases, 0);
        assert.deepEqual(JSON.parse(configGetAll(`uzi-retained.${owner}.journal`)[0]!).journal, s.journal);
        assert.equal(fs.existsSync(s.pred.clonePath), false, "missing owner source is never reconstructed");
        assert.equal(treeHash(s.sibling), s.hash);
        assert.equal(pins(), s.pinSnapshot);
        assert.ok(lines.some(l => (l as { msg?: string }).msg === "orphan_reclaim_succeeded"));
      });
    }

    for (const refusal of ["nonterminal", "404", "503", "quiescence"] as const) {
      it(`missing-source retained ${attempt ? "attempt" : "canonical"} ${refusal} refusal`, async t => {
        const iid = 25133, owner = randomUUID(), claimant = randomUUID();
        const s = await missingPrimary(iid, owner, attempt, true);
        const envelope = JSON.stringify({ version: 1, branch: `agent/issue-${iid}`, key: `issue-${iid}`, journal: s.journal });
        command("config", `uzi-retained.${owner}.journal`, envelope);
        if (refusal === "404") api.setOrphanNotFound(owner);
        else if (refusal === "503") api.failOrphanClassification(owner, 503);
        else if (refusal === "quiescence") terminalOwner(owner, iid);
        else api.setOrphanClassification(owner, {
          status: "running", repo_id: "r1", kind: "issue", issue_iid: iid,
          branch: null, pipeline_ref: null, pipeline_id: null,
        });
        let releases = 0, seeded = 0;
        for (const method of ["releaseRecoveryCustody", "settleRecoveryHold", "settleRecoveryHoldLive"] as const) {
          t.mock.method(client, method, async () => { releases++; throw new Error("unexpected custody release"); });
        }
        const { factory, started } = transientFactory();
        const worker = restartedWorker(factory, {
          dockerHost: undefined,
          quiesceRun: async req => req.site === "orphan_reclaim" && refusal === "quiescence" ? {
            process: { state: "survivors", processes: [], killed: [], detail: "retained sibling writer" },
            docker: { state: "not_wired", removed: [], detail: "" },
          } : fastQuiesce(req),
        });
        const seed = worker.git.runnerCloneForBranch.bind(worker.git);
        t.mock.method(worker.git, "runnerCloneForBranch", async (...args: Parameters<typeof seed>) => {
          const clone = await seed(...args);
          seeded++;
          return clone;
        });
        const paths = fs.readdirSync(runnerRepoDir()).sort();
        const ledger = configGetAll(`uzi-attempts.agent/issue-${iid}.entry`);
        assert.equal(await boundedExecute(worker.runner, gitlabClaim(iid, { run_id: claimant })), false);
        assert.equal(started(), 0);
        assert.equal(seeded, 0);
        assert.equal(releases, 0);
        assert.deepEqual(readJournal(iid), s.journal);
        assert.deepEqual(configGetAll(`uzi-retained.${owner}.journal`), [envelope]);
        assert.deepEqual(configGetAll(`uzi-attempts.agent/issue-${iid}.entry`), ledger);
        assert.equal(pins(), s.pinSnapshot);
        assert.equal(treeHash(s.sibling), s.hash);
        assert.deepEqual(fs.readdirSync(runnerRepoDir()).sort(), paths);
      });
    }

    it(`missing-source own ${attempt ? "attempt" : "canonical"} runner refusal`, async t => {
      const iid = 25132, owner = randomUUID();
      const s = await missingPrimary(iid, owner, attempt, false);
      const envelope = JSON.stringify({ version: 1, branch: `agent/issue-${iid}`, key: `issue-${iid}`, journal: s.journal });
      command("config", `uzi-retained.${owner}.journal`, envelope);
      let releases = 0;
      for (const method of ["releaseRecoveryCustody", "settleRecoveryHold", "settleRecoveryHoldLive"] as const) {
        t.mock.method(client, method, async () => { releases++; throw new Error("unexpected custody release"); });
      }
      const { factory, started } = transientFactory();
      const worker = restartedWorker(factory, attempt ? {} : { dockerHost: undefined });
      const paths = fs.readdirSync(runnerRepoDir()).sort();
      assert.equal(await boundedExecute(worker.runner, gitlabClaim(iid, { run_id: owner, claim_generation: 2 })), false);
      assert.equal(started(), 0);
      assert.equal(releases, 0);
      const after = readJournal(iid)!;
      assert.equal(after.runId, owner);
      assert.equal(after.clonePath, s.pred.clonePath);
      assert.equal(after.attemptId, s.pred.attemptId);
      assert.deepEqual(after.retainedSources, s.journal.retainedSources);
      assert.deepEqual(after.recovery?.source, { runId: owner, clonePath: s.pred.clonePath, ...(s.pred.attemptId ? { attemptId: s.pred.attemptId } : {}) });
      assert.equal(after.recovery?.blocker, "source_missing");
      assert.deepEqual(configGetAll(`uzi-retained.${owner}.journal`), [envelope]);
      assert.equal(pins(), s.pinSnapshot);
      assert.equal(treeHash(s.sibling), s.hash);
      assert.deepEqual(fs.readdirSync(runnerRepoDir()).sort(), paths);
    });
  }
});

describe("Unit 2 terminal retained descriptors", { skip: !HAS_PROCFS }, () => {
  async function retained(iid: number, attempt: boolean) {
    const owner = randomUUID();
    const pred = await seedPredecessor(iid, owner, { attempt });
    await git.reserveRecoveryIteration(bare(), `agent/issue-${iid}`, `issue-${iid}`, { runId: owner, ...pred }, 5);
    return { owner, pred, journal: readJournal(iid)! };
  }

  for (const attempt of [true, false]) {
    it(`terminal retained ${attempt ? "attempt" : "canonical"} detaches custody and permits next issue run`, async () => {
      const iid = attempt ? 25121 : 25122;
      const { owner, pred, journal } = await retained(iid, attempt);
      terminalOwner(owner, iid);
      const { factory, started } = transientFactory(ctx => {
        assert.notEqual(ctx.worktreePath, pred.clonePath);
        assert.ok(parseAttemptPath(ctx.worktreePath, path.join(fx.dataDir, "runner")));
        assert.notEqual(readJournal(iid)?.runId, owner);
      });
      for (let generation = 2; generation <= 3; generation++) {
        const worker = restartedWorker(factory, { dockerHost: undefined });
        assert.equal(await boundedExecute(worker.runner, gitlabClaim(iid, { claim_generation: generation })), false);
        assert.equal(started(), generation - 1, "actual next executor entry");
        const protectedRaw = configGetAll(`uzi-retained.${owner}.journal`);
        assert.equal(protectedRaw.length, 1);
        assert.deepEqual(JSON.parse(protectedRaw[0]!), { version: 1, branch: `agent/issue-${iid}`, key: `issue-${iid}`, journal });
        assert.equal(fs.readFileSync(path.join(pred.clonePath, "ONLY_COPY.txt"), "utf8"), "must survive recovery\n");
        if (pred.attemptId) assert.equal(readLedger(iid).get(pred.attemptId)?.state, "reclaimed");
        const inventory = await worker.git.readInventoryCloneHeads(bare(), owner);
        assert.equal(inventory.kind, "verified");
        if (inventory.kind === "verified") assert.ok(inventory.clones.some(c => c.clonePath === pred.clonePath));
      }
    });
  }

  it("live retained owner refuses descriptor detachment", async () => {
    const iid = 25123;
    const { owner, journal } = await retained(iid, true);
    const { factory, started } = transientFactory();
    await restartedWorker(factory).runner.execute(gitlabClaim(iid));
    assert.equal(started(), 0);
    assert.deepEqual(readJournal(iid), journal);
    assert.deepEqual(configGetAll(`uzi-retained.${owner}.journal`), []);
  });

  for (const canonical of [false, true]) {
    it(canonical ? "unwired canonical writer refuses terminal descriptor detachment" : "retained sibling writer refuses terminal descriptor detachment", async () => {
      const iid = canonical ? 25125 : 25124;
      const { owner, pred, journal } = await retained(iid, !canonical);
      let writerPath = pred.clonePath;
      if (!canonical) {
        const siblingId = mintAttemptId(1);
        const sibling = await git.createOrAttachRunnerClone(bare(), iid + 1, noProofReseed, owner, false, undefined, fixtureSeed(siblingId));
        // Attribute a same-key predecessor with a real clone and matching ledger.
        writerPath = `${canonicalFor(iid)}.attempt-${siblingId}`;
        fs.renameSync(sibling.path, writerPath);
        execFileSync("git", ["-C", bare(), "config", "--local", "--unset-all", `uzi-attempts.agent/issue-${iid + 1}.entry`], { env: GIT_ENV });
        execFileSync("git", ["-C", bare(), "config", "--local", "--add", `uzi-attempts.agent/issue-${iid}.entry`,
          JSON.stringify({ attemptId: siblingId, runId: owner, clonePath: writerPath, state: "live" })], { env: GIT_ENV });
        journal.retainedSources = [{ runId: owner, clonePath: writerPath, attemptId: siblingId }];
        execFileSync("git", ["-C", bare(), "config", "--local", `uzi-recovery.agent/issue-${iid}.clone`, JSON.stringify(journal)], { env: GIT_ENV });
      }
      terminalOwner(owner, iid);
      const { factory, started } = transientFactory();
      const requests: QuiesceRunRequest[] = [];
      const quiesceRun = async (req: QuiesceRunRequest): Promise<QuiesceRunOutcome> => {
        requests.push(req);
        if (req.targetPaths?.includes(writerPath)) return {
          process: { state: "survivors", processes: [], killed: [], detail: "writer still present" },
          docker: { state: "not_wired", removed: [], detail: "" },
        };
        return fastQuiesce(req);
      };
      await restartedWorker(factory, { dockerHost: undefined, quiesceRun }).runner.execute(gitlabClaim(iid));
      assert.ok(requests.some(r => r.targetPaths?.includes(writerPath)), "proof includes writer path");
      assert.equal(started(), 0);
      assert.deepEqual(readJournal(iid), journal);
      assert.deepEqual(configGetAll(`uzi-retained.${owner}.journal`), []);
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
