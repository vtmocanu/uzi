// issue #1783 (R0/R3/R5 + Docker teardown) — prove a run's clone is QUIESCENT before the worker
// parks it, retires it, or finalizes from it.
//
// killAgentTree SIGKILLs only the CLI process groups the SDK executor recorded, and confirms
// nothing. A tool shell the CLI starts detached (its own group), or anything the agent backgrounds
// with `setsid`, survives it, keeps its cwd inside the clone, and can keep writing there — or keep
// a Docker container alive whose bind mounts point into the clone — while the worker fetches the
// branch back, publishes a checkpoint or removes the tree. This module closes that gap:
//
//   - The PROCESS REAPER scans the process table for runner-uid processes whose env or cwd ties
//     them to this run's clone, SIGKILLs the ones this run owns, rescans until the scan is empty,
//     and reports `quiescent`, `survivors` or `unverified`. Both non-quiescent results fail
//     closed at every caller: the worker never treats "could not prove it" as "proved it".
//   - The DOCKER TEARDOWN force-removes every container with a bind mount under the clone, then
//     lists until two consecutive listings are clean. It never reports quiescence: a create the
//     daemon already accepted can still complete after the last listing, so its best result is
//     `docker_unconfirmed`.
//
// Attribution (in order):
//   R0  Only the runner uid (10002 under the uid split; this process's uid single-uid) is
//       considered, read from the per-pid `status` file FIRST. A pid that vanishes between steps
//       is skipped silently. A runner-uid pid whose env or cwd cannot be read (EACCES — a
//       non-dumpable process) is attributed by ancestry through the world-readable `stat`: a
//       ppid chain, process group or session leading to (or being) a recorded root of ANOTHER
//       live attempt (a Claude CLI group or a Codex provider supervisor), or a long-lived
//       runner-uid root the worker itself launched and recorded, is out of scope; anything else
//       is `unverified`.
//   R3  SCOPE first. A process is in scope iff its UZI_RUN_CLONE_KEY equals the target key or its
//       cwd lies (by whole path components) within a target path. Out of scope is ignored
//       entirely, so two unrelated healthy runs never interact. In scope, by marker: this
//       attempt's → kill; a LIVE other attempt of the key → never signalled, reported as a
//       conflict; a terminal attempt's → kill; no marker → kill in mode `own`, but a survivor in
//       modes `seed`/`capture` (no positive attribution there). A process carrying this worker's
//       spawn mark (UZI_WORKER_SPAWN) and NO attempt marker is the worker's own op and is never
//       run-owned.
//   R5  The scanner never signals itself or any ancestor up to pid 1, nor a descendant of its own
//       unless that descendant carries an attempt marker. The exception matters single-uid, where
//       the scanner is the worker itself: the agent CLI is a worker child, so a marked descendant
//       is agent residue, not the scanner's. The runner-uid helper starts no agent, so it
//       excludes every descendant (its TypeScript loader may run a service child).
//
// Under the uid split the worker cannot read or signal a runner-uid process, so the scan and the
// kills run AS the runner, in a helper: this same module re-executed through runnerCommand with
// an explicit env (the worker mark, never an attempt marker), cwd "/", a fresh private TMPDIR and
// the tsx cache off. It reads its request on stdin and prints exactly one JSON line. Single-uid,
// the scan runs in-process. Every process-supplied string (comm, cwd, container id) is sanitized
// before it is logged or reaches a failure reason.

import http from "node:http";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { spawn, spawnSync } from "node:child_process";
import { randomBytes } from "node:crypto";
import { fileURLToPath } from "node:url";
import { cloneKeyOf, formatAttemptId, isWithinPath } from "./attempt-path.js";
import { parseDockerTarget } from "./docker-wiring.js";
import { RUNNER_UID, killRunnerGroup, runnerCommand, runnerPath, runnerTmpdir, uidSplitActive } from "./runner-uid.js";
import type { SdkAttemptEnv } from "./sdk-env.js";
import {
  RUN_ATTEMPT_ENV,
  RUN_CLONE_KEY_ENV,
  WORKER_SPAWN_ENV,
  workerRunnerRootPids,
  workerSpawnEnv,
  workerSpawnNonce,
} from "./worker-spawn-mark.js";

// ─── Log sanitization ──────────────────────────────────────────────────────────────────────

/** C0/C1 controls, DEL, and the bidi embedding/override/isolate/mark code points. */
function unsafeCodePoint(c: number): boolean {
  return (
    c < 0x20 ||
    (c >= 0x7f && c <= 0x9f) ||
    c === 0x061c ||
    c === 0x200e ||
    c === 0x200f ||
    (c >= 0x202a && c <= 0x202e) ||
    (c >= 0x2066 && c <= 0x2069)
  );
}

/**
 * A process-supplied string (a comm, a cwd, a container id, a helper detail) made safe to log or
 * to put into a failure reason: every control and bidi code point becomes `?`, and the result is
 * capped at `max` characters. A runner-uid process chooses its own comm and cwd, so neither may
 * reach a log line or the run's failure_reason raw.
 */
export function sanitizeForLog(text: string, max = 256): string {
  let out = "";
  for (const ch of String(text)) {
    out += unsafeCodePoint(ch.codePointAt(0) ?? 0) ? "?" : ch;
    if (out.length >= max) return `${out.slice(0, max)}...`;
  }
  return out;
}

// ─── Attempts ──────────────────────────────────────────────────────────────────────────────

/** One execution attempt of a run: the marker its agent CLI carries and the CLI roots it spawned. */
export interface RunAttempt extends SdkAttemptEnv {
  runId: string;
  attemptId: string;
  /** The recorded CLI root pids (process-group leaders) of this attempt not yet reaped. */
  recordedRootPids: () => number[];
}

/** Mint a fresh attempt for `runId` running in `clonePath`. */
export function newRunAttempt(
  runId: string,
  claimGeneration: number | undefined,
  clonePath: string,
  recordedRootPids: () => number[],
  now: Date = new Date(),
): RunAttempt {
  const attemptId = formatAttemptId(now, claimGeneration, randomBytes(8).toString("hex"));
  return {
    runId,
    attemptId,
    marker: `${runId}:${attemptId}`,
    clonePath: path.resolve(clonePath),
    cloneKey: cloneKeyOf(clonePath).cloneKey,
    recordedRootPids,
  };
}

/** The in-process set of LIVE attempts on this worker, keyed by marker. An attempt is added
 *  before its CLI spawns and removed after that attempt's final reap. */
export class LiveAttemptRegistry {
  private readonly attempts = new Map<string, RunAttempt>();

  add(attempt: RunAttempt): void {
    this.attempts.set(attempt.marker, attempt);
  }

  remove(marker: string): void {
    this.attempts.delete(marker);
  }

  /** Every live attempt except the one with `marker`. */
  others(marker: string | undefined): RunAttempt[] {
    return [...this.attempts.values()].filter((a) => a.marker !== marker);
  }
}

// ─── Process table ─────────────────────────────────────────────────────────────────────────

/** The per-pid reads the reaper makes. Every method throws an errno-coded error on failure. */
export interface ProcTable {
  listPids(): number[];
  readStatus(pid: number): string;
  readEnviron(pid: number): string;
  readCwd(pid: number): string;
  readStat(pid: number): string;
}

const PROC_ROOT = path.join("/", "proc");

/** The real process table (Linux procfs). */
export const procfsTable: ProcTable = {
  listPids: () =>
    fs
      .readdirSync(PROC_ROOT)
      .filter((n) => /^[0-9]+$/.test(n))
      .map(Number),
  readStatus: (pid) => fs.readFileSync(path.join(PROC_ROOT, String(pid), "status"), "utf8"),
  readEnviron: (pid) => fs.readFileSync(path.join(PROC_ROOT, String(pid), "environ"), "latin1"),
  readCwd: (pid) => fs.readlinkSync(path.join(PROC_ROOT, String(pid), "cwd")),
  readStat: (pid) => fs.readFileSync(path.join(PROC_ROOT, String(pid), "stat"), "utf8"),
};

type ReadOutcome<T> = { ok: true; value: T } | { ok: false; vanished: boolean };

function tryRead<T>(fn: () => T): ReadOutcome<T> {
  try {
    return { ok: true, value: fn() };
  } catch (err) {
    const code = (err as NodeJS.ErrnoException).code;
    return { ok: false, vanished: code === "ENOENT" || code === "ESRCH" };
  }
}

interface StatusFacts {
  uid: number;
  comm: string;
  zombie: boolean;
}

function parseStatus(text: string): StatusFacts | undefined {
  const uidLine = /^Uid:\s+(\d+)/m.exec(text);
  if (!uidLine) return undefined;
  const name = /^Name:\s*(.*)$/m.exec(text)?.[1] ?? "?";
  const state = /^State:\s*(\S)/m.exec(text)?.[1];
  return { uid: Number(uidLine[1]), comm: sanitizeForLog(name, 64), zombie: state === "Z" || state === "X" };
}

interface StatFacts {
  ppid: number;
  pgid: number;
  sid: number;
}

function parseStat(text: string): StatFacts | undefined {
  // `pid (comm) state ppid pgrp session …` — comm may itself contain ")" and spaces.
  const close = text.lastIndexOf(")");
  if (close < 0) return undefined;
  const fields = text.slice(close + 2).trim().split(/\s+/);
  const ppid = Number(fields[1]);
  const pgid = Number(fields[2]);
  const sid = Number(fields[3]);
  if (![ppid, pgid, sid].every(Number.isInteger)) return undefined;
  return { ppid, pgid, sid };
}

function parseEnviron(text: string): Map<string, string> {
  const env = new Map<string, string>();
  for (const entry of text.split("\u0000")) {
    const eq = entry.indexOf("=");
    if (eq > 0) env.set(entry.slice(0, eq), entry.slice(eq + 1));
  }
  return env;
}

/** A cwd whose directory was removed reads back with this suffix. */
function stripDeleted(cwd: string): string {
  return cwd.endsWith(" (deleted)") ? cwd.slice(0, -" (deleted)".length) : cwd;
}

// ─── Scan ──────────────────────────────────────────────────────────────────────────────────

/** `own`: the run's own teardown (unmarked in-scope processes are killed). `seed`/`capture`:
 *  a sweep before seeding or capturing a clone (an unmarked in-scope process is a survivor). */
export type QuiesceMode = "own" | "seed" | "capture";

/** What the scan is looking for. JSON-serializable: it crosses into the runner-uid helper. */
export interface ScanRequest {
  mode: QuiesceMode;
  /** Only processes of this real uid are considered. */
  targetUid: number;
  /** `<repoDir>/<key>` of the clone being quiesced. */
  targetKey: string;
  /** Absolute paths whose subtree is the clone's footprint. */
  targetPaths: string[];
  /** This attempt's marker (absent for a sweep with no own attempt). */
  ownMarker?: string;
  /** Markers of every OTHER live attempt on this worker. */
  liveMarkers: string[];
  /** Recorded root pids of every OTHER live attempt on this worker (Claude CLI groups, Codex
   *  provider supervisors), plus the long-lived runner-uid roots the worker itself launched. */
  liveRootPids: number[];
  /** This worker's spawn nonce: a process carrying it is never run-owned. */
  workerNonce: string;
}

/** One listed process in a result. */
export interface QuiesceProcess {
  pid: number;
  uid: number;
  comm: string;
  cwd: string;
  reason: string;
}

interface ScanResult {
  kill: QuiesceProcess[];
  survivors: QuiesceProcess[];
  unverified: QuiesceProcess[];
  tableError?: string;
}

/** The chain of `pid`'s ancestors (excluding pid itself), from the world-readable stat. */
function ancestorsOf(pid: number, table: ProcTable): number[] {
  const out: number[] = [];
  let cur = pid;
  for (let depth = 0; depth < 256; depth++) {
    const r = tryRead(() => table.readStat(cur));
    if (!r.ok) break;
    const facts = parseStat(r.value);
    if (!facts || facts.ppid <= 0 || out.includes(facts.ppid)) break;
    out.push(facts.ppid);
    if (facts.ppid === 1) break;
    cur = facts.ppid;
  }
  return out;
}

/** True when an unreadable pid IS, or leads by ppid chain, process group or session to, a recorded
 *  root of another live attempt or a worker-launched runner-uid root. */
function attributedToOtherLive(pid: number, table: ProcTable, roots: ReadonlySet<number>): boolean {
  if (roots.size === 0) return false;
  if (roots.has(pid)) return true;
  const r = tryRead(() => table.readStat(pid));
  if (r.ok) {
    const facts = parseStat(r.value);
    if (facts && (roots.has(facts.pgid) || roots.has(facts.sid))) return true;
  }
  return ancestorsOf(pid, table).some((a) => roots.has(a));
}

/** One pass over the process table. Pure apart from the reads; signals nothing. */
export function scanOnce(
  req: ScanRequest,
  table: ProcTable,
  selfPid: number = process.pid,
  excludeAllDescendants = false,
): ScanResult {
  const result: ScanResult = { kill: [], survivors: [], unverified: [] };
  let pids: number[];
  try {
    pids = table.listPids();
  } catch (err) {
    result.tableError = `process table unreadable: ${(err as Error).message}`;
    return result;
  }
  // R5: never self, never an ancestor; a descendant only when an attempt marker proves residue.
  const excluded = new Set<number>([selfPid, ...ancestorsOf(selfPid, table)]);
  const parentOf = new Map<number, number>();
  for (const pid of pids) {
    const r = tryRead(() => table.readStat(pid));
    const facts = r.ok ? parseStat(r.value) : undefined;
    if (facts) parentOf.set(pid, facts.ppid);
  }
  const isDescendant = (pid: number): boolean => {
    let cur = parentOf.get(pid);
    for (let depth = 0; cur !== undefined && cur > 1 && depth < 256; depth++) {
      if (cur === selfPid) return true;
      cur = parentOf.get(cur);
    }
    return false;
  };
  const liveMarkers = new Set(req.liveMarkers);
  const roots = new Set(req.liveRootPids);

  for (const pid of pids) {
    if (excluded.has(pid)) continue;
    const descendant = isDescendant(pid);
    // R0: the owning uid FIRST, from the world-readable status file.
    const st = tryRead(() => table.readStatus(pid));
    if (!st.ok) {
      if (!st.vanished) {
        result.unverified.push({ pid, uid: -1, comm: "?", cwd: "unreadable", reason: "status_unreadable" });
      }
      continue;
    }
    const status = parseStatus(st.value);
    if (!status) {
      result.unverified.push({ pid, uid: -1, comm: "?", cwd: "unreadable", reason: "status_unparsable" });
      continue;
    }
    if (status.uid !== req.targetUid || status.zombie) continue;
    const envRead = tryRead(() => table.readEnviron(pid));
    const cwdRead = tryRead(() => table.readCwd(pid));
    if ((!envRead.ok && envRead.vanished) || (!cwdRead.ok && cwdRead.vanished)) continue;
    if (!envRead.ok || !cwdRead.ok) {
      // A non-dumpable runner-uid process: attribute by ancestry, else fail closed.
      if (attributedToOtherLive(pid, table, roots)) continue;
      result.unverified.push({
        pid,
        uid: status.uid,
        comm: status.comm,
        cwd: cwdRead.ok ? sanitizeForLog(cwdRead.value) : "unreadable",
        reason: "unreadable_unattributed",
      });
      continue;
    }
    const env = parseEnviron(envRead.value);
    // The worker mark exempts a process only when it carries NO attempt marker: a worker spawn
    // never has one (workerSpawnEnv strips it), so an attempt-marked process that also claims the
    // nonce is agent residue that copied it, and is classified like any other.
    if (env.get(WORKER_SPAWN_ENV) === req.workerNonce && env.get(RUN_ATTEMPT_ENV) === undefined) continue;
    // R5: a descendant is the scanner's own unless an attempt marker says it is agent residue.
    if (descendant && (excludeAllDescendants || env.get(RUN_ATTEMPT_ENV) === undefined)) continue;
    const cwd = cwdRead.value;
    const inScope =
      env.get(RUN_CLONE_KEY_ENV) === req.targetKey ||
      req.targetPaths.some((p) => isWithinPath(stripDeleted(cwd), p));
    if (!inScope) continue;
    const entry = (reason: string): QuiesceProcess => ({
      pid,
      uid: status.uid,
      comm: status.comm,
      cwd: sanitizeForLog(cwd),
      reason,
    });
    const marker = env.get(RUN_ATTEMPT_ENV);
    if (marker !== undefined && marker === req.ownMarker) result.kill.push(entry("own_attempt"));
    else if (marker !== undefined && liveMarkers.has(marker)) result.survivors.push(entry("live_attempt_conflict"));
    else if (marker !== undefined) result.kill.push(entry("terminal_attempt"));
    else if (req.mode === "own") result.kill.push(entry("unmarked_in_scope"));
    else result.survivors.push(entry("unattributed_in_scope"));
  }
  return result;
}

// ─── Reap loop ─────────────────────────────────────────────────────────────────────────────

export type ProcessQuiescenceState = "quiescent" | "survivors" | "unverified";

/** The process half's verdict. `survivors` and `unverified` both fail closed at every caller. */
export interface ProcessQuiescence {
  state: ProcessQuiescenceState;
  /** The processes behind a non-quiescent verdict. */
  processes: QuiesceProcess[];
  /** Pids SIGKILLed during the reap. */
  killed: number[];
  detail: string;
}

const REAP_DEADLINE_MS = 5_000;
const REAP_INTERVAL_MS = 100;
const HELPER_TIMEOUT_MS = 10_000;

/** Injectable seams for {@link reapProcesses}. */
export interface ReapDeps {
  table?: ProcTable;
  kill?: (pid: number) => void;
  sleep?: (ms: number) => Promise<void>;
  now?: () => number;
  deadlineMs?: number;
  intervalMs?: number;
  selfPid?: number;
  /** The runner-uid helper excludes EVERY descendant: it starts no agent, so anything below it
   *  (e.g. its own TypeScript loader's service process, which inherits its env) is its own. */
  excludeAllDescendants?: boolean;
}

const defaultSleep = (ms: number) => new Promise<void>((r) => setTimeout(r, ms));

function defaultKill(pid: number): void {
  try {
    process.kill(pid, "SIGKILL");
  } catch {
    // ESRCH: already gone. Anything else: the rescan finds it again and reports it.
  }
}

/**
 * The in-process reap loop: scan, SIGKILL every match, rescan every 100 ms until a scan has
 * nothing left to kill and nothing unattributable, or the 5 s deadline passes; then classify the
 * final scan.
 */
export async function reapProcesses(req: ScanRequest, deps: ReapDeps = {}): Promise<ProcessQuiescence> {
  const table = deps.table ?? procfsTable;
  const kill = deps.kill ?? defaultKill;
  const sleep = deps.sleep ?? defaultSleep;
  const now = deps.now ?? Date.now;
  const deadline = now() + (deps.deadlineMs ?? REAP_DEADLINE_MS);
  const selfPid = deps.selfPid ?? process.pid;
  const killed: number[] = [];
  let last: ScanResult;
  for (;;) {
    last = scanOnce(req, table, selfPid, deps.excludeAllDescendants ?? false);
    if (last.tableError) {
      return { state: "unverified", processes: [], killed, detail: last.tableError };
    }
    // An unreadable pid is rescanned too: a process caught mid-execve (or mid-exit) can read
    // back EACCES for an instant. Only one that stays unattributable to the deadline counts.
    if ((last.kill.length === 0 && last.unverified.length === 0) || now() >= deadline) break;
    for (const p of last.kill) {
      kill(p.pid);
      killed.push(p.pid);
    }
    await sleep(deps.intervalMs ?? REAP_INTERVAL_MS);
  }
  const stuck = last.kill.map((p) => ({ ...p, reason: `${p.reason}:kill_unconfirmed` }));
  if (last.unverified.length > 0) {
    return {
      state: "unverified",
      processes: [...last.unverified, ...last.survivors, ...stuck],
      killed,
      detail: `${last.unverified.length} runner-uid process(es) could not be attributed`,
    };
  }
  if (last.survivors.length > 0 || stuck.length > 0) {
    return {
      state: "survivors",
      processes: [...last.survivors, ...stuck],
      killed,
      detail: `${last.survivors.length + stuck.length} in-scope process(es) survive the reap`,
    };
  }
  return { state: "quiescent", processes: [], killed, detail: `reaped ${killed.length} process(es)` };
}

// ─── Runner-uid helper ─────────────────────────────────────────────────────────────────────

const HELPER_FLAG = "--uzi-quiesce-helper";
const HELPER_FILE = fileURLToPath(import.meta.url);
/** The helper's stdout is bounded: more than this is not a verdict, it is a failure. */
const HELPER_STDOUT_MAX = 1024 * 1024;
/** The request the helper reads from stdin is bounded the same way. */
const HELPER_REQUEST_MAX = 1024 * 1024;
const HELPER_PATH = "/usr/local/bin:/usr/bin:/bin";

/** How the helper is started; injectable so a test can force a spawn failure. The request is
 *  written to the child's stdin, never its argv: a process's cmdline is world-readable. */
export type HelperSpawn = (
  command: string,
  args: string[],
  opts: { cwd: string; env: NodeJS.ProcessEnv },
) => import("node:child_process").ChildProcess;

const defaultHelperSpawn: HelperSpawn = (command, args, opts) =>
  spawn(command, args, { cwd: opts.cwd, env: opts.env, detached: true, stdio: ["pipe", "pipe", "pipe"] });

/** The helper's private TMPDIR: made fresh (mode 0700, owned by the uid the helper runs as)
 *  before every helper start and removed after it. Injectable for tests. */
export interface HelperTmp {
  make(): string;
  remove(dir: string): void;
}

const defaultHelperTmp: HelperTmp = {
  make: () => {
    const base = runnerTmpdir() ?? os.tmpdir();
    // Single-uid the helper runs as this process's uid, so a plain mkdtemp (mode 0700) is its own.
    if (!uidSplitActive()) return fs.mkdtempSync(path.join(base, "uzi-quiesce-"));
    // Under the split the dir must be the RUNNER's, so mktemp runs as the runner (mode 0700).
    const w = runnerCommand("mktemp", ["-d", path.join(base, "uzi-quiesce-XXXXXXXXXX")]);
    const r = spawnSync(w.command, w.args, {
      env: workerSpawnEnv({ PATH: HELPER_PATH }),
      encoding: "utf8",
      stdio: ["ignore", "pipe", "ignore"],
      timeout: 5_000,
    });
    const dir = String(r.stdout ?? "").trim();
    if (r.status !== 0 || !path.isAbsolute(dir) || !isWithinPath(dir, base) || path.resolve(dir) === path.resolve(base)) {
      throw new Error(`mktemp as the runner exited ${String(r.status)}`);
    }
    return dir;
  },
  remove: (dir) => {
    if (!uidSplitActive()) {
      fs.rmSync(dir, { recursive: true, force: true });
      return;
    }
    const w = runnerCommand("rm", ["-rf", "--", dir]);
    spawnSync(w.command, w.args, { env: workerSpawnEnv({ PATH: HELPER_PATH }), stdio: "ignore", timeout: 10_000 });
  },
};

function helperArgv(): string[] {
  // The helper runs this TypeScript module, so it needs the tsx loader. The loader is resolved to
  // an absolute URL here because the helper's cwd is "/", where a bare `tsx` would not resolve.
  return ["--import", import.meta.resolve("tsx"), HELPER_FILE, HELPER_FLAG];
}

function isProcessQuiescence(v: unknown): v is ProcessQuiescence {
  if (typeof v !== "object" || v === null) return false;
  const o = v as Record<string, unknown>;
  return (
    (o.state === "quiescent" || o.state === "survivors" || o.state === "unverified") &&
    Array.isArray(o.processes) &&
    o.processes.every((p) => typeof p === "object" && p !== null && Number.isInteger((p as { pid?: unknown }).pid)) &&
    Array.isArray(o.killed) &&
    o.killed.every((n) => Number.isInteger(n)) &&
    typeof o.detail === "string"
  );
}

/** A helper verdict with every string field sanitized: it crossed a uid boundary. */
function sanitizeVerdict(v: ProcessQuiescence): ProcessQuiescence {
  return {
    state: v.state,
    processes: v.processes.map((p) => ({
      pid: p.pid,
      uid: Number.isInteger(p.uid) ? p.uid : -1,
      comm: sanitizeForLog(String(p.comm), 64),
      cwd: sanitizeForLog(String(p.cwd)),
      reason: sanitizeForLog(String(p.reason), 64),
    })),
    killed: v.killed,
    detail: sanitizeForLog(v.detail),
  };
}

/**
 * The helper's verdict from its whole stdout: exactly ONE non-empty line, and that line a
 * well-formed verdict. No line, several lines, or a malformed line is `unverified`: a second
 * line could be a forged verdict racing the real one, and taking either would trust it.
 */
function parseHelperVerdict(stdout: string): ProcessQuiescence {
  const unverified = (detail: string): ProcessQuiescence => ({ state: "unverified", processes: [], killed: [], detail });
  const lines = stdout.split("\n").filter((l) => l.trim() !== "");
  if (lines.length === 0) return unverified("quiescence helper answered no JSON");
  if (lines.length > 1) return unverified(`quiescence helper answered ${lines.length} lines, not one verdict`);
  let parsed: unknown;
  try {
    parsed = JSON.parse(lines[0]!);
  } catch {
    return unverified("quiescence helper answered no JSON");
  }
  return isProcessQuiescence(parsed) ? sanitizeVerdict(parsed) : unverified("quiescence helper answered malformed JSON");
}

/**
 * Run the reap loop in a helper started through `runnerCommand` (setpriv to the runner under
 * the uid split; a passthrough single-uid), with an explicit env carrying the worker mark and no
 * attempt marker, cwd "/", a fresh private TMPDIR, the tsx cache disabled (a cache the agent can
 * plant would hand the helper forged code), the request on stdin, a bounded stdout, and a 10 s
 * spawn timeout. Any failure to start, finish or answer is `unverified`.
 */
async function reapProcessesViaHelper(
  req: ScanRequest,
  opts: { spawnHelper?: HelperSpawn; timeoutMs?: number; helperTmp?: HelperTmp } = {},
): Promise<ProcessQuiescence> {
  const unverified = (detail: string): ProcessQuiescence => ({ state: "unverified", processes: [], killed: [], detail });
  let argv: string[];
  try {
    argv = helperArgv();
  } catch (err) {
    return unverified(`quiescence helper unavailable: ${(err as Error).message}`);
  }
  const helperTmp = opts.helperTmp ?? defaultHelperTmp;
  let tmpDir: string;
  try {
    tmpDir = helperTmp.make();
  } catch (err) {
    return unverified(`quiescence helper TMPDIR unavailable: ${(err as Error).message}`);
  }
  const removeTmp = (): void => {
    try {
      helperTmp.remove(tmpDir);
    } catch {
      // Best effort: a leftover private dir is harmless.
    }
  };
  const wrapped = runnerCommand(process.execPath, argv);
  const helperEnv: NodeJS.ProcessEnv = { PATH: runnerPath() ?? HELPER_PATH, TMPDIR: tmpDir, TSX_DISABLE_CACHE: "1" };
  return new Promise<ProcessQuiescence>((resolve) => {
    let settled = false;
    let timer: NodeJS.Timeout | undefined;
    const done = (r: ProcessQuiescence): void => {
      if (settled) return;
      settled = true;
      if (timer) clearTimeout(timer);
      removeTmp();
      resolve(r);
    };
    let child: import("node:child_process").ChildProcess;
    try {
      child = (opts.spawnHelper ?? defaultHelperSpawn)(wrapped.command, wrapped.args, {
        cwd: "/",
        env: workerSpawnEnv(helperEnv),
      });
    } catch (err) {
      done(unverified(`quiescence helper spawn failed: ${(err as Error).message}`));
      return;
    }
    timer = setTimeout(() => {
      killRunnerGroup(child.pid);
      done(unverified("quiescence helper timed out"));
    }, opts.timeoutMs ?? HELPER_TIMEOUT_MS);
    const out: Buffer[] = [];
    let outBytes = 0;
    child.stdout?.on("data", (c: Buffer) => {
      if (settled) return;
      outBytes += c.length;
      if (outBytes > HELPER_STDOUT_MAX) {
        killRunnerGroup(child.pid);
        done(unverified("quiescence helper output exceeded 1 MiB"));
        return;
      }
      out.push(c);
    });
    child.stderr?.resume();
    // A helper that dies before reading its request fails this write (EPIPE); the close handler
    // below reports that exit, so the write error itself is only swallowed here.
    child.stdin?.on("error", () => undefined);
    child.stdin?.end(JSON.stringify(req));
    child.on("error", (err) => done(unverified(`quiescence helper spawn failed: ${err.message}`)));
    child.on("close", (code, signal) => {
      if (code !== 0) {
        done(unverified(`quiescence helper exited ${code ?? signal ?? "abnormally"}`));
        return;
      }
      done(parseHelperVerdict(Buffer.concat(out).toString("utf8")));
    });
  });
}

/** Options for {@link reapRunProcesses}. */
export interface ReapRunOptions extends ReapDeps {
  /** Run the scan in the runner-uid helper (default: when the uid split is active). */
  viaHelper?: boolean;
  spawnHelper?: HelperSpawn;
  helperTimeoutMs?: number;
  helperTmp?: HelperTmp;
}

/** The uid whose processes a reap considers: the runner under the split, else this process's. */
function defaultTargetUid(): number {
  return uidSplitActive() ? RUNNER_UID : (process.getuid?.() ?? RUNNER_UID);
}

/** Reap in-process or through the helper (see {@link reapProcessesViaHelper}). */
export async function reapRunProcesses(req: ScanRequest, opts: ReapRunOptions = {}): Promise<ProcessQuiescence> {
  const viaHelper = opts.viaHelper ?? uidSplitActive();
  if (viaHelper) {
    return reapProcessesViaHelper(req, {
      spawnHelper: opts.spawnHelper,
      timeoutMs: opts.helperTimeoutMs,
      helperTmp: opts.helperTmp,
    });
  }
  try {
    return await reapProcesses(req, opts);
  } catch (err) {
    return { state: "unverified", processes: [], killed: [], detail: `reap failed: ${(err as Error).message}` };
  }
}

// ─── Docker teardown ───────────────────────────────────────────────────────────────────────

export type DockerTeardownState = "not_wired" | "docker_unconfirmed" | "docker_error";

/** The Docker half's result. Never `quiescent`: an accepted create can still complete later. */
export interface DockerTeardown {
  state: DockerTeardownState;
  /** Ids of the containers force-removed. */
  removed: string[];
  detail: string;
}

/** Injectable seams for {@link teardownDocker}. */
export interface DockerTeardownOptions {
  dockerHost: string | undefined;
  targetPaths: string[];
  requestTimeoutMs?: number;
  /** Per-response body cap (default 8 MiB); above it the request is destroyed → docker_error. */
  maxBodyBytes?: number;
  intervalMs?: number;
  deadlineMs?: number;
  sleep?: (ms: number) => Promise<void>;
  now?: () => number;
}

type Endpoint = { socketPath: string } | { host: string; port: number };

/** Every DOCKER_HOST form docker-wiring accepts (its one shared parser): a bare `/path` socket,
 *  `unix://<path>`, and `tcp://host[:port]` (no port ⇒ Docker's default 2375). */
function parseDockerHost(dockerHost: string): Endpoint | undefined {
  const target = parseDockerTarget(dockerHost);
  if (!target) return undefined;
  return "path" in target ? { socketPath: target.path } : { host: target.host, port: target.port };
}

/** A Docker Engine response body larger than this is refused (the request is destroyed). */
const DOCKER_BODY_MAX = 8 * 1024 * 1024;

function dockerRequest(
  ep: Endpoint,
  method: string,
  urlPath: string,
  timeoutMs: number,
  maxBodyBytes: number = DOCKER_BODY_MAX,
): Promise<{ status: number; body: string }> {
  return new Promise((resolve, reject) => {
    const req = http.request({ ...ep, method, path: urlPath, timeout: timeoutMs }, (res) => {
      const chunks: Buffer[] = [];
      let bytes = 0;
      res.on("data", (c: Buffer) => {
        bytes += c.length;
        if (bytes > maxBodyBytes) {
          // Reject FIRST (the promise settles once), then tear the connection down.
          reject(new Error(`docker ${method} response body exceeded ${maxBodyBytes} bytes`));
          res.destroy();
          req.destroy();
          return;
        }
        chunks.push(c);
      });
      res.on("end", () => resolve({ status: res.statusCode ?? 0, body: Buffer.concat(chunks).toString("utf8") }));
      res.on("error", reject);
    });
    req.on("timeout", () => req.destroy(new Error(`docker ${method} ${urlPath} timed out`)));
    req.on("error", reject);
    req.end();
  });
}

function boundContainers(body: string, targetPaths: string[]): string[] {
  const parsed: unknown = JSON.parse(body);
  if (!Array.isArray(parsed)) throw new Error("container listing is not an array");
  const ids: string[] = [];
  for (const c of parsed) {
    if (typeof c !== "object" || c === null) continue;
    const id = (c as { Id?: unknown }).Id;
    const mounts = (c as { Mounts?: unknown }).Mounts;
    if (typeof id !== "string" || !Array.isArray(mounts)) continue;
    const bound = mounts.some((m) => {
      const src = (m as { Source?: unknown } | null)?.Source;
      return typeof src === "string" && src !== "" && targetPaths.some((t) => isWithinPath(src, t));
    });
    if (bound) ids.push(id);
  }
  return ids;
}

/**
 * Force-remove every container with a mount source under a target path, then keep listing (1 s
 * apart) until two consecutive listings are clean, within 15 s overall and 5 s per request.
 * Containers binding nothing under a target path are never touched.
 *
 * Out of scope (disclosed): a container binding an ANCESTOR of the clone (e.g. the whole runner
 * root) is not removed, because it may belong to a sibling run.
 */
export async function teardownDocker(opts: DockerTeardownOptions): Promise<DockerTeardown> {
  if (!opts.dockerHost) return { state: "not_wired", removed: [], detail: "no docker daemon wired" };
  const ep = parseDockerHost(opts.dockerHost);
  if (!ep) return { state: "docker_error", removed: [], detail: "unsupported DOCKER_HOST form" };
  const sleep = opts.sleep ?? defaultSleep;
  const now = opts.now ?? Date.now;
  const reqTimeout = opts.requestTimeoutMs ?? 5_000;
  const deadline = now() + (opts.deadlineMs ?? 15_000);
  const removed = new Set<string>();
  let clean = 0;
  try {
    for (;;) {
      const list = await dockerRequest(ep, "GET", "/containers/json?all=1", reqTimeout, opts.maxBodyBytes);
      if (list.status !== 200) throw new Error(`container listing returned HTTP ${list.status}`);
      const ids = boundContainers(list.body, opts.targetPaths);
      if (ids.length === 0) {
        clean += 1;
        if (clean >= 2) {
          return {
            state: "docker_unconfirmed",
            removed: [...removed],
            detail: `removed ${removed.size} container(s); two consecutive clean listings`,
          };
        }
      } else {
        clean = 0;
        for (const id of ids) {
          const del = await dockerRequest(
            ep,
            "DELETE",
            `/containers/${encodeURIComponent(id)}?force=1`,
            reqTimeout,
            opts.maxBodyBytes,
          );
          if (del.status === 204 || del.status === 200 || del.status === 404) removed.add(sanitizeForLog(id, 128));
        }
      }
      if (now() >= deadline) {
        return {
          state: "docker_error",
          removed: [...removed],
          detail: "deadline passed before two consecutive clean listings",
        };
      }
      await sleep(opts.intervalMs ?? 1_000);
    }
  } catch (err) {
    return { state: "docker_error", removed: [...removed], detail: sanitizeForLog((err as Error).message) };
  }
}

// ─── Orchestration ─────────────────────────────────────────────────────────────────────────

/** One quiescence request for a run's clone. */
export interface QuiesceRunRequest {
  mode: QuiesceMode;
  /** This attempt, when there is one (its marker is "own"). */
  attempt: RunAttempt | undefined;
  /** `<repoDir>/<key>` being quiesced. */
  cloneKey: string;
  /** The clone footprint: this attempt's path, the key's canonical path, and (for a sweep) the
   *  key's attempt paths. */
  targetPaths: string[];
  /** false skips the process half (a Codex run: its supervisor already proves process drain). */
  processes: boolean;
  dockerHost: string | undefined;
  registry: LiveAttemptRegistry;
}

/** The two halves' results. `process` is undefined when the process half was skipped. */
export interface QuiesceRunOutcome {
  process: ProcessQuiescence | undefined;
  docker: DockerTeardown;
}

/** Injectable seams for {@link quiesceRunAttempt}. */
export interface QuiesceRunDeps {
  reap?: ReapRunOptions;
  docker?: Omit<DockerTeardownOptions, "dockerHost" | "targetPaths">;
  targetUid?: number;
}

/** The process reap (unless skipped), then the Docker teardown. Never throws. */
export async function quiesceRunAttempt(req: QuiesceRunRequest, deps: QuiesceRunDeps = {}): Promise<QuiesceRunOutcome> {
  let processResult: ProcessQuiescence | undefined;
  if (req.processes) {
    const others = req.registry.others(req.attempt?.marker);
    const scan: ScanRequest = {
      mode: req.mode,
      targetUid: deps.targetUid ?? defaultTargetUid(),
      targetKey: req.cloneKey,
      targetPaths: req.targetPaths.map((p) => path.resolve(p)),
      ownMarker: req.attempt?.marker,
      liveMarkers: others.map((a) => a.marker),
      // Other live attempts' recorded roots (Claude CLI groups, Codex provider supervisors) plus
      // every long-lived runner-uid root this worker launched and recorded itself.
      liveRootPids: [
        ...new Set([
          ...others.flatMap((a) => {
            try {
              return a.recordedRootPids();
            } catch {
              return [];
            }
          }),
          ...workerRunnerRootPids(),
        ]),
      ],
      workerNonce: workerSpawnNonce(),
    };
    processResult = await reapRunProcesses(scan, deps.reap);
  }
  const docker = await teardownDocker({ ...deps.docker, dockerHost: req.dockerHost, targetPaths: req.targetPaths }).catch(
    (err: unknown): DockerTeardown => ({ state: "docker_error", removed: [], detail: (err as Error).message }),
  );
  return { process: processResult, docker };
}

// ─── Helper entry ──────────────────────────────────────────────────────────────────────────

function isScanRequest(v: unknown): v is ScanRequest {
  if (typeof v !== "object" || v === null) return false;
  const o = v as Record<string, unknown>;
  const strings = (x: unknown) => Array.isArray(x) && x.every((s) => typeof s === "string");
  return (
    (o.mode === "own" || o.mode === "seed" || o.mode === "capture") &&
    Number.isInteger(o.targetUid) &&
    typeof o.targetKey === "string" &&
    strings(o.targetPaths) &&
    (o.ownMarker === undefined || typeof o.ownMarker === "string") &&
    strings(o.liveMarkers) &&
    Array.isArray(o.liveRootPids) &&
    o.liveRootPids.every((n) => Number.isInteger(n)) &&
    typeof o.workerNonce === "string"
  );
}

/** The helper's request, read from stdin (bounded). */
async function readHelperRequest(): Promise<string> {
  const chunks: Buffer[] = [];
  let bytes = 0;
  for await (const c of process.stdin) {
    const buf = Buffer.isBuffer(c) ? c : Buffer.from(String(c));
    bytes += buf.length;
    if (bytes > HELPER_REQUEST_MAX) throw new Error("helper request exceeds 1 MiB");
    chunks.push(buf);
  }
  return Buffer.concat(chunks).toString("utf8");
}

async function helperMain(): Promise<ProcessQuiescence> {
  let req: unknown;
  try {
    req = JSON.parse(await readHelperRequest());
  } catch {
    return { state: "unverified", processes: [], killed: [], detail: "helper request unparsable" };
  }
  if (!isScanRequest(req)) return { state: "unverified", processes: [], killed: [], detail: "helper request invalid" };
  return reapProcesses(req, { excludeAllDescendants: true });
}

if (process.argv[2] === HELPER_FLAG && process.argv[1] && path.resolve(process.argv[1]) === HELPER_FILE) {
  helperMain().then(
    (r) => {
      process.stdout.write(`${JSON.stringify(r)}\n`, () => process.exit(0));
    },
    (err: unknown) => {
      const r: ProcessQuiescence = { state: "unverified", processes: [], killed: [], detail: `helper failed: ${(err as Error).message}` };
      process.stdout.write(`${JSON.stringify(r)}\n`, () => process.exit(0));
    },
  );
}

/** Log line fields for every process a non-quiescent result lists. */
export function describeProcesses(q: ProcessQuiescence | undefined): Array<Record<string, unknown>> {
  return (q?.processes ?? []).map((p) => ({
    pid: p.pid,
    uid: p.uid,
    comm: sanitizeForLog(p.comm, 64),
    cwd: sanitizeForLog(p.cwd),
    reason: sanitizeForLog(p.reason, 64),
  }));
}
