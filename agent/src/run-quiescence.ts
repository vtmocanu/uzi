import { AsyncLocalStorage } from "node:async_hooks";
import type { BoundaryProcessHandle, BoundaryProcessRequest } from "./harness.js";
import { TrustedExecutionRefusal } from "./trusted-execution-refusal.js";

interface Scope {
  readonly attempt: SdkAttemptEnv | undefined;
  readonly signal: AbortSignal;
  readonly deadlineAt: number;
  readonly live: () => boolean;
  readonly spawn: (request: BoundaryProcessRequest) => Promise<BoundaryProcessHandle>;
  readonly track: (pending: Promise<unknown>) => void;
  readonly owner: object;
  readonly enterExclusive: () => object;
  readonly leaveExclusive: (token: object) => void;
  readonly validateExclusive: (token: object, request?: BoundaryProcessRequest) => void;
  accepting: boolean;
}
type ReaperOperation = "create" | "scan" | "remove";
interface ReaperTempOwner {
  readonly base: string;
  readonly scanPath: string;
  created?: string;
  removed: boolean;
}

const scopes = new AsyncLocalStorage<Scope>();
const requests = new WeakMap<BoundaryProcessRequest, { scope: Scope; pipeline: { running: boolean; token: object }; input: string; deadlineAt: number; canonical: string; owner: ReaperTempOwner; kind: ReaperOperation; signal: AbortSignal }>();

/** Safety owns activation and revocation; inherited stores remain refusal markers. */
export function reaperCaptureScope(
  options: Omit<Scope, "accepting">,
): { run<T>(action: () => Promise<T>): Promise<T>; revoke(): void } {
  const scope: Scope = { ...options, accepting: true };
  return {
    run: action => scopes.run(scope, action),
    revoke: () => { scope.accepting = false; },
  };
}

function hasReaperCaptureScope(): boolean { return scopes.getStore() !== undefined; }

/** The reaper supplies only its privately constructed fixed operations. */
function runClosedReaper<T>(
  attempt: SdkAttemptEnv | undefined,
  action: (lane: {
    readonly signal: AbortSignal;
    readonly deadlineAt: number;
    spawn(request: BoundaryProcessRequest, input: string, deadlineAt: number, owner: ReaperTempOwner, kind: ReaperOperation, signal: AbortSignal): Promise<BoundaryProcessHandle>;
  }) => Promise<T>,
): Promise<T> {
  const scope = scopes.getStore();
  const checkScope = () => {
    if (!scope || !scope.live() || scope.signal.aborted || Date.now() >= scope.deadlineAt) {
      throw new TrustedExecutionRefusal("reaper capture scope unavailable");
    }
  };
  checkScope();
  if (!scope!.accepting || !attempt || !scope!.attempt ||
      scope!.attempt.marker !== attempt.marker || scope!.attempt.cloneKey !== attempt.cloneKey ||
      scope!.attempt.clonePath !== attempt.clonePath) {
    throw new TrustedExecutionRefusal("reaper attempt does not belong to this capture");
  }
  // Claim synchronously before the first pipeline effect; siblings fail immediately.
  const pipeline = { running: true, token: scope!.enterExclusive() };
  const children: Promise<unknown>[] = [];
  // Register the full pipeline before invoking any code, including synchronous failures.
  const pending = Promise.resolve().then(() => action({
    signal: scope!.signal,
    deadlineAt: scope!.deadlineAt,
    spawn: (request, input, deadlineAt, owner, kind, signal) => {
      checkScope();
      if (!pipeline.running) throw new TrustedExecutionRefusal("reaper pipeline revoked");
      if (!Number.isFinite(deadlineAt) || deadlineAt > scope!.deadlineAt || Date.now() >= deadlineAt) {
        throw new TrustedExecutionRefusal("invalid reaper deadline");
      }
      scope!.validateExclusive(pipeline.token);
      requests.set(request, { scope: scope!, pipeline, input, deadlineAt, owner, kind, signal,
        canonical: JSON.stringify(request) });
      assertClosedReaperRequest(request);
      const child = scope!.spawn(request);
      // Include startup and full root completion even when the action ignores its handle.
      const completed = child.then(handle => handle.completed);
      children.push(completed);
      void completed.catch(() => undefined);
      return child;
    },
  })).finally(async () => {
    pipeline.running = false;
    // No stage can start after revocation. Each admitted child has its existing deadline;
    // join every sibling even if one fails before releasing safety's exclusive guard.
    try { await Promise.allSettled(children); }
    finally { scope!.leaveExclusive(pipeline.token); }
  });
  scope!.track(pending);
  void pending.catch(() => undefined);
  return pending;
}

export function closedReaperDeadline(request: BoundaryProcessRequest): number {
  assertClosedReaperRequest(request);
  return requests.get(request)!.deadlineAt;
}

export function isClosedReaperRequest(request: BoundaryProcessRequest): boolean {
  return requests.has(request);
}

export function assertClosedReaperRequest(request: BoundaryProcessRequest, signal?: AbortSignal, owner?: object): string {
  const bound = requests.get(request);
  if (!bound || (signal && signal !== bound.scope.signal) || (owner && owner !== bound.scope.owner)) {
    throw new TrustedExecutionRefusal("unbound reaper operation");
  }
  if (!bound.pipeline.running || !bound.scope.live() || bound.scope.signal.aborted ||
      Date.now() >= bound.scope.deadlineAt) {
    throw new TrustedExecutionRefusal("reaper capture scope unavailable");
  }
  bound.scope.validateExclusive(bound.pipeline.token, request);
  bound.signal.throwIfAborted();
  const argv = bound.kind === "create"
    ? ["/bin/mktemp", "-d", path.join(bound.owner.base, "uzi-quiesce-XXXXXXXXXX")]
    : bound.kind === "remove" ? ["/bin/rm", "-rf", "--", bound.owner.created!]
    : [process.execPath, ...helperArgv()];
  const env = workerSpawnEnv(bound.kind === "scan"
    ? { PATH: bound.owner.scanPath, TMPDIR: bound.owner.created!, TSX_DISABLE_CACHE: "1" }
    : { PATH: HELPER_PATH });
  // Only the private fixed builder below can populate requests. Validate the actual
  // complete launch envelope again at registry admission and immediately at spawn.
  if (!Object.isFrozen(request) || !Object.isFrozen(request.argv) || !Object.isFrozen(request.env) ||
      JSON.stringify(request) !== bound.canonical ||
      JSON.stringify(request.argv) !== JSON.stringify(argv) ||
      JSON.stringify(request.env) !== JSON.stringify(env) ||
      (bound.kind !== "scan" && bound.input !== "") ||
      !Number.isFinite(request.timeoutMs) || request.timeoutMs! <= 0 ||
      request.cwd !== "/" || request.identity !== "command" ||
      request.recoverableTimeout !== undefined || Date.now() >= bound.deadlineAt ||
      (bound.kind !== "create" && (!bound.owner.created || bound.owner.removed)) ||
      (bound.kind === "remove" && request.argv[3] !== bound.owner.created) ||
      (bound.kind === "scan" && request.env.TMPDIR !== bound.owner.created)) {
    throw new TrustedExecutionRefusal("noncanonical reaper operation");
  }
  return bound.input;
}

import { MaintenanceBudget } from "./maintenance-budget.js";
import { MaintenanceResources } from "./recovery-maintenance.js";
import type { Readable } from "node:stream";

/** Reserve raw bytes before removing them from a scoped readable. No short-read refunds.
 * Each callback retains the admitting scope even for injected emitters outside ALS.
 * Failure pauses the stream and cancels its owner; settlement remains the owner's close. */
function pullMaintenance(stream: Readable | null, cap: number, deliver: (chunk: Buffer) => void,
  failure: () => void): void {
  if (!stream) return;
  const within = AsyncLocalStorage.snapshot();
  let bytes = 0, stopped = false;
  const pull = () => within(() => {
    if (stopped) return;
    try {
      MaintenanceResources.checkIfActive();
      while (stream.readableLength > 0) {
        const length = Math.min(stream.readableLength, 64 * 1024, cap - bytes);
        if (length <= 0) throw new Error("maintenance output envelope");
        MaintenanceResources.consumeIfActive("metadataBytes", length);
        bytes += length;
        const chunk: unknown = stream.read(length);
        if (chunk === null) return;
        if (!Buffer.isBuffer(chunk)) throw new Error("maintenance raw stream required");
        MaintenanceResources.checkIfActive();
        deliver(chunk);
      }
      // A drained buffer needs one more zero-length read for the stream to observe EOF and
      // emit 'end'; without it a complete response never settles and waits for its timeout.
      stream.read(0);
    } catch {
      stopped = true;
      stream.pause();
      failure();
    }
  });
  stream.on("readable", pull);
  pull();
}

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
//   - The DOCKER TEARDOWN force-removes every container with a bind mount under the clone, with
//     its anonymous volumes (`v=1`; named volumes survive), then lists until two consecutive listings are clean. It never reports quiescence: a create the
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
//       is `unverified` unless the solitary uid-split permission and a stable stat start time
//       permit reaping it. A root is recorded as its pid AND its start time (`stat` field 22,
//       captured at spawn/registration) and matches only a live process with both, so a pid the
//       kernel recycled after the root exited never exempts anything.
//   R3  SCOPE first. A process is in scope iff its cwd lies (by whole path components) within a
//       target path, or it carries BOTH a well-formed attempt marker (UZI_RUN_ATTEMPT,
//       `<runId>:<attemptId>`) AND a UZI_RUN_CLONE_KEY equal to the target key. The key alone
//       never scopes a process: a runner-uid process of another run can carry any key with no
//       marker (`env -u UZI_RUN_ATTEMPT UZI_RUN_CLONE_KEY=<key> setsid …` from /tmp) and would
//       otherwise be an unattributed survivor wedging that key's every seed until the pod
//       restarts. Such a process, with its cwd outside every target path, is ignored. Deliberate
//       env forgery (a copied marker) and evasion (a cwd outside the clone with no marker) stay
//       out of scope of this proof. Out of scope is ignored entirely, so two unrelated healthy
//       runs never interact. In scope, by marker: this
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
import { spawn } from "node:child_process";
import { randomBytes } from "node:crypto";
import { fileURLToPath } from "node:url";
import { maintenanceEsbuildExecutable } from "./maintenance-bootstrap-executable.js";
import { ATTEMPT_ID_RE, cloneKeyOf, formatAttemptId, isWithinPath } from "./attempt-path.js";
import { parseDockerTarget } from "./docker-wiring.js";
import { RUNNER_UID, killRunnerGroup, runnerCommand, runnerPath, runnerTmpdir, uidSplitActive } from "./runner-uid.js";
import type { SdkAttemptEnv } from "./sdk-env.js";
import {
  RUN_ATTEMPT_ENV,
  RUN_CLONE_KEY_ENV,
  WORKER_SPAWN_ENV,
  statStartTime,
  workerRunnerRootPids,
  workerSpawnEnv,
  workerSpawnNonce,
  type RecordedRoot,
} from "./worker-spawn-mark.js";

// ─── Log sanitization ──────────────────────────────────────────────────────────────────────

/** C0/C1 controls, DEL, the U+2028/U+2029 line/paragraph separators (a line break to a JSON log
 *  reader or a JS-string sink), and the bidi embedding/override/isolate/mark code points. */
function unsafeCodePoint(c: number): boolean {
  return (
    c < 0x20 ||
    (c >= 0x7f && c <= 0x9f) ||
    c === 0x061c ||
    c === 0x200e ||
    c === 0x200f ||
    c === 0x2028 ||
    c === 0x2029 ||
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
  // Iterating a string yields whole code points, so a surrogate pair is appended or dropped as a
  // unit (never split), and `...` marks only a string that really had more to say.
  for (const ch of String(text)) {
    const safe = unsafeCodePoint(ch.codePointAt(0) ?? 0) ? "?" : ch;
    if (out.length + safe.length > max) return `${out}...`;
    out += safe;
  }
  return out;
}

// ─── Attempts ──────────────────────────────────────────────────────────────────────────────

/** One execution attempt of a run: the marker its agent CLI carries and the CLI roots it spawned. */
export interface RunAttempt extends SdkAttemptEnv {
  runId: string;
  attemptId: string;
  /** The recorded CLI roots (process-group leaders, pid + start time) of this attempt not yet
   *  reaped. */
  recordedRootPids: () => RecordedRoot[];
}

const workerAttempts = new WeakMap<RunAttempt, string>();

/** Mint a fresh attempt id: the attempt's start time, its claim generation, 64 random bits. */
export function mintAttemptId(claimGeneration: number | undefined, now: Date = new Date()): string {
  return formatAttemptId(now, claimGeneration, randomBytes(8).toString("hex"));
}

/** Mint a fresh attempt for `runId` running in `clonePath`. `attemptId` (issue #1783 M2) reuses
 *  the id a Docker-wired worker already minted to name the attempt's clone path, so the marker,
 *  the path and the recovery journal all carry one id. */
export function newRunAttempt(
  runId: string,
  claimGeneration: number | undefined,
  clonePath: string,
  recordedRootPids: () => RecordedRoot[],
  now: Date = new Date(),
  attemptId: string = mintAttemptId(claimGeneration, now),
): RunAttempt {
  if (!ATTEMPT_ID_RE.test(attemptId)) throw new Error("malformed attempt id");
  const attempt = {
    runId,
    attemptId,
    marker: `${runId}:${attemptId}`,
    clonePath: path.resolve(clonePath),
    cloneKey: cloneKeyOf(clonePath).cloneKey,
    recordedRootPids,
  };
  workerAttempts.set(attempt, JSON.stringify([attempt.marker, attempt.cloneKey, attempt.clonePath]));
  return attempt;
}

/** True for a marker of newRunAttempt's shape, `<runId>:<attemptId>`: a non-empty run id, then
 *  (after the LAST colon) an attempt id matching {@link ATTEMPT_ID_RE}. */
function wellFormedAttemptMarker(marker: string | undefined): boolean {
  if (marker === undefined) return false;
  const i = marker.lastIndexOf(":");
  return i > 0 && ATTEMPT_ID_RE.test(marker.slice(i + 1));
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

  /** True when `clonePath` is the clone path of a live attempt (compared resolved). */
  isLivePath(clonePath: string): boolean {
    const p = path.resolve(clonePath);
    return [...this.attempts.values()].some((a) => a.clonePath === p);
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
  /** Kernel executable identity, used only by exact bootstrap exemptions. */
  readExecutable?(pid: number): string;
  /** This table's own proc mountinfo, used only by the exclusive-idle proof: a hidepid-
   *  restricted mount hides other processes, so it can never prove idle. */
  readProcMountInfo?(): string;
}

const PROC_ROOT = path.join("/", "proc");
const meteredProcTables = new WeakSet<ProcTable>();
const helperMeteredProcTables = new WeakMap<ProcTable, HelperWork | undefined>();
const HELPER_WORK_LIMITS = { adminEntries: 512, metadataBytes: 32 * 1024 * 1024 };
type HelperWorkLimits = typeof HELPER_WORK_LIMITS;
const PRODUCER_WORK_LIMITS = { adminEntries: 128, metadataBytes: 16 * 1024 * 1024 };
/** Exclusive-idle rescans absorb a transient worker helper; all within the caller's envelope. */
const EXCLUSIVE_IDLE_MAX_SCANS = 3;
const EXCLUSIVE_IDLE_INTERVAL_MS = 250;

function validateProducerWorkLimits(limits: unknown, mode: QuiesceMode, workerWide?: boolean, exclusiveIdle?: boolean): void {
  if (limits === undefined) return;
  if (!MaintenanceResources.active() || (MaintenanceResources.discoveryActive() && exclusiveIdle !== true) ||
      mode !== "capture" || workerWide === true || !isHelperWorkLimits(limits) ||
      limits.adminEntries <= 0 || limits.adminEntries > PRODUCER_WORK_LIMITS.adminEntries ||
      limits.metadataBytes <= 0 || limits.metadataBytes > PRODUCER_WORK_LIMITS.metadataBytes)
    throw new Error("invalid producer quiescence envelope");
  if (exclusiveIdle === true) MaintenanceResources.checkIfActive();
  else MaintenanceResources.check();
}
type HelperScanRequest = ScanRequest & { workLimits?: HelperWorkLimits };

/** One sticky allowance for all raw proc work in one child invocation, including rescans.
 * Failed reservations consume no more work; earlier reservations are never refunded. */
class HelperWork {
  private readonly left: HelperWorkLimits;
  failed = false;
  constructor(limits: HelperWorkLimits) { this.left = { ...limits }; }
  check(): void { if (this.failed) throw new Error("helper work exhausted"); }
  consume(resource: keyof HelperWorkLimits, amount: number): void {
    this.check();
    if (amount > this.left[resource]) {
      this.failed = true;
      this.check();
    }
    this.left[resource] -= amount;
  }
}
const helperWork = new AsyncLocalStorage<HelperWork>();

/** A procfs-shaped table rooted at `root`: `<root>/<pid>/{status,environ,cwd,stat}`. */
export function procfsTableAt(root: string, budget?: { signal?: AbortSignal; deadline: number }): ProcTable {
  const work = helperWork.getStore();
  if (budget || MaintenanceResources.active() || work) {
    let total = 0;
    const check = () => {
      work?.check();
      MaintenanceResources.checkIfActive();
      budget?.signal?.throwIfAborted();
      if (budget && Date.now() >= budget.deadline) throw new Error("process proof deadline");
    };
    const read = (pid: number | "self", name: string, cap: number, encoding: BufferEncoding) => {
      check();
      const fd = fs.openSync(path.join(root, String(pid), name), fs.constants.O_RDONLY |
        fs.constants.O_NOFOLLOW | fs.constants.O_NONBLOCK);
      try {
        work?.consume("metadataBytes", cap + 1);
        MaintenanceResources.consumeIfActive("metadataBytes", cap + 1);
        const buf = Buffer.alloc(cap + 1);
        let length = 0;
        // Complete EOF, including at the cap; at most cap+1 nonempty reads.
        while (length < buf.length) {
          check();
          const count = fs.readSync(fd, buf, length, buf.length - length, null);
          if (!count) break;
          length += count;
          total += count;
          if (total > 32 * 1024 * 1024) throw new Error("process byte budget");
        }
        if (length > cap) throw new Error("process file budget");
        return buf.toString(encoding, 0, length);
      } finally { fs.closeSync(fd); }
    };
    const table: ProcTable = {
      listPids: () => {
        check();
        const pids: number[] = [];
        const directory = fs.opendirSync(root);
        try {
          for (;;) {
            check();
            work?.consume("adminEntries", 1);
            MaintenanceResources.consumeIfActive("adminEntries", 1);
            const entry = directory.readSync();
            if (!entry) break;
            if (!/^[0-9]+$/.test(entry.name)) continue;
            if (pids.length >= 4096) throw new Error("process PID budget");
            pids.push(Number(entry.name));
          }
        } finally { directory.closeSync(); }
        return pids;
      },
      readStatus: pid => read(pid, "status", 64 * 1024, "utf8"),
      readStat: pid => read(pid, "stat", 64 * 1024, "utf8"),
      readEnviron: pid => read(pid, "environ", 256 * 1024, "latin1"),
      readCwd: pid => {
        check();
        work?.consume("metadataBytes", 64 * 1024);
        MaintenanceResources.consumeIfActive("metadataBytes", 64 * 1024);
        const cwd = fs.readlinkSync(path.join(root, String(pid), "cwd"));
        if (Buffer.byteLength(cwd) > 64 * 1024) throw new Error("process cwd budget");
        return cwd;
      },
      readExecutable: pid => {
        check();
        work?.consume("metadataBytes", 64 * 1024);
        MaintenanceResources.consumeIfActive("metadataBytes", 64 * 1024);
        const executable = fs.readlinkSync(path.join(root, String(pid), "exe"));
        if (Buffer.byteLength(executable) > 64 * 1024) throw new Error("process executable budget");
        return executable;
      },
      readProcMountInfo: () => read("self", "mountinfo", 256 * 1024, "utf8"),
    };
    meteredProcTables.add(table);
    helperMeteredProcTables.set(table, work);
    return table;
  }
  // Ordinary killing retains the shipped, unbounded legacy procfs reads.
  // Only the report-budget branch above changes the read policy.
  const table: ProcTable = {
    listPids: () =>
      fs
        .readdirSync(root)
        .filter((n) => /^[0-9]+$/.test(n))
        .map(Number),
    readStatus: (pid) => fs.readFileSync(path.join(root, String(pid), "status"), "utf8"),
    readEnviron: (pid) => fs.readFileSync(path.join(root, String(pid), "environ"), "latin1"),
    readCwd: (pid) => fs.readlinkSync(path.join(root, String(pid), "cwd")),
    readStat: (pid) => fs.readFileSync(path.join(root, String(pid), "stat"), "utf8"),
    readExecutable: (pid) => fs.readlinkSync(path.join(root, String(pid), "exe")),
    readProcMountInfo: () => fs.readFileSync(path.join(root, "self", "mountinfo"), "utf8"),
  };
  helperMeteredProcTables.set(table, work);
  return table;
}

/** The real process table (Linux procfs). */
export const procfsTable: ProcTable = procfsTableAt(PROC_ROOT);

// ─── Test view ─────────────────────────────────────────────────────────────────────────────

/**
 * TEST ONLY: what a reap sees instead of the host's whole process table. JSON-serializable, so it
 * crosses into the helper with the request. Either a FAKE procfs-shaped root (a "kill" removes
 * `<procRoot>/<pid>`, so a fake pid never signals a real process), or the REAL procfs narrowed to
 * the descendants of one pid (plus pids the test registered, directly or in pidfiles read at every
 * listing: a `setsid` plant is reparented away from the test), killed for real. The worker installs
 * one only through {@link setQuiescenceViewForTests} (no environment variable or config reads one,
 * because a knob that narrows the scan would be a fail-open switch), and production code never
 * calls that setter. The runner-uid helper is the one other reader: it honours whatever `view` its
 * stdin request carries, and that stdin is the worker's own pipe to the child it spawned, so the
 * worker forwards a view only when a test set one.
 */
export type QuiescenceView =
  | { procRoot: string; deadlineMs?: number; intervalMs?: number }
  | { descendantsOf: number; extraPids?: number[]; extraPidFiles?: string[]; deadlineMs?: number; intervalMs?: number };

/** A view's reap deadline stays strictly below the helper's own timeout (HELPER_TIMEOUT_MS), so a
 *  helper reaping a view answers before the worker gives up on it. */
const VIEW_DEADLINE_MAX_MS = 5_000;
/** A view's rescan interval is at least 1 ms (0 would spin the reap loop) and at most the deadline. */
const VIEW_INTERVAL_MIN_MS = 1;
/** At most this much of a view's pidfile is read at each listing. */
const VIEW_PIDFILE_MAX_BYTES = 64 * 1024;
const VIEW_EXTRA_PIDS_MAX = 1024;
const FAKE_VIEW_DEADLINE_MS = 200;
const FAKE_VIEW_INTERVAL_MS = 10;

let testView: QuiescenceView | undefined;

const viewTiming = (v: unknown, min: number): boolean =>
  v === undefined || (Number.isSafeInteger(v) && (v as number) >= min && (v as number) <= VIEW_DEADLINE_MAX_MS);
const positivePid = (v: unknown): boolean => Number.isSafeInteger(v) && (v as number) > 0;

/** True for a well-formed view whose fake root (if any) is an absolute path that is not, and does
 *  not resolve into, the real proc root. */
function isQuiescenceView(v: unknown): v is QuiescenceView {
  if (typeof v !== "object" || v === null) return false;
  const o = v as Record<string, unknown>;
  if (!viewTiming(o.deadlineMs, 0) || !viewTiming(o.intervalMs, VIEW_INTERVAL_MIN_MS)) return false;
  if ("procRoot" in o) {
    if (typeof o.procRoot !== "string" || !path.isAbsolute(o.procRoot) || "descendantsOf" in o) return false;
    let real = path.resolve(o.procRoot);
    try {
      real = fs.realpathSync(real);
    } catch {
      // A missing root is not the real one; its listing fails and the reap is unverified.
    }
    return !isWithinPath(path.resolve(o.procRoot), PROC_ROOT) && !isWithinPath(real, PROC_ROOT);
  }
  return (
    positivePid(o.descendantsOf) &&
    (o.extraPids === undefined ||
      (Array.isArray(o.extraPids) && o.extraPids.length <= VIEW_EXTRA_PIDS_MAX && o.extraPids.every(positivePid))) &&
    (o.extraPidFiles === undefined ||
      (Array.isArray(o.extraPidFiles) &&
        o.extraPidFiles.length <= VIEW_EXTRA_PIDS_MAX &&
        o.extraPidFiles.every((f) => typeof f === "string" && path.isAbsolute(f))))
  );
}

/** TEST ONLY: install (or, with undefined, clear) the view every reap without an explicit `table`
 *  uses, in-process and forwarded to the helper. Throws on a malformed view. */
export function setQuiescenceViewForTests(view: QuiescenceView | undefined): void {
  if (view !== undefined && !isQuiescenceView(view)) throw new Error("invalid quiescence test view");
  testView = view === undefined ? undefined : structuredClone(view);
}

/** The pids listed one per line in the first {@link VIEW_PIDFILE_MAX_BYTES} of `file` (none when it
 *  cannot be read). */
function pidsInFile(file: string): number[] {
  let fd: number | undefined;
  try {
    fd = fs.openSync(file, "r");
    const work = helperWork.getStore();
    const cap = VIEW_PIDFILE_MAX_BYTES + (work ? 1 : 0);
    work?.consume("metadataBytes", cap);
    const buf = Buffer.alloc(cap);
    let len = 0;
    for (;;) {
      const n = fs.readSync(fd, buf, len, buf.length - len, null);
      if (n === 0) break;
      len += n;
      if (len === buf.length) break;
    }
    work?.check();
    let text = buf.toString("utf8", 0, Math.min(len, VIEW_PIDFILE_MAX_BYTES));
    // A file cut at the cap drops its last, possibly truncated, line (a cut "12345" is not "123").
    if (len >= VIEW_PIDFILE_MAX_BYTES) text = text.slice(0, text.lastIndexOf("\n") + 1);
    return text
      .split("\n")
      .filter((l) => /^\d+$/.test(l.trim()))
      .map(Number);
  } catch {
    return [];
  } finally {
    try {
      if (fd !== undefined) fs.closeSync(fd);
    } catch {
      // nothing to recover: the pids were already read
    }
  }
}

/** The real procfs listing only `root`'s descendants (by the world-readable ppid), `extraPids` and
 *  the pids in `extraPidFiles` (re-read at every listing). */
function descendantsTable(root: number, extraPids: readonly number[], extraPidFiles: readonly string[]): ProcTable {
  const raw = helperWork.getStore() ? procfsTableAt(PROC_ROOT) : procfsTable;
  return {
    ...raw,
    listPids: () => {
      const pids = raw.listPids();
      const extra = new Set([...extraPids, ...extraPidFiles.flatMap(pidsInFile)]);
      const parentOf = new Map<number, number>();
      for (const pid of pids) {
        const r = tryRead(() => raw.readStat(pid));
        const facts = r.ok ? parseStat(r.value) : undefined;
        if (facts) parentOf.set(pid, facts.ppid);
      }
      const under = (pid: number): boolean => {
        let cur = parentOf.get(pid);
        for (let depth = 0; cur !== undefined && cur > 0 && depth < 256; depth++) {
          if (cur === root) return true;
          cur = parentOf.get(cur);
        }
        return false;
      };
      helperWork.getStore()?.check();
      return pids.filter((pid) => extra.has(pid) || under(pid));
    },
  };
}

/** The table, kill and timings a view stands for. */
function viewReapDeps(view: QuiescenceView): Required<Pick<ReapDeps, "table" | "kill" | "deadlineMs" | "intervalMs">> {
  if ("procRoot" in view) {
    const root = view.procRoot;
    return {
      table: procfsTableAt(root),
      kill: (pid) => fs.rmSync(path.join(root, String(pid)), { recursive: true, force: true }),
      deadlineMs: view.deadlineMs ?? FAKE_VIEW_DEADLINE_MS,
      intervalMs: view.intervalMs ?? FAKE_VIEW_INTERVAL_MS,
    };
  }
  return {
    table: descendantsTable(view.descendantsOf, view.extraPids ?? [], view.extraPidFiles ?? []),
    kill: defaultKill,
    deadlineMs: view.deadlineMs ?? REAP_DEADLINE_MS,
    intervalMs: view.intervalMs ?? REAP_INTERVAL_MS,
  };
}

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
  /** The comm between the first "(" and the LAST ")", sanitized (a process chooses its own). */
  comm: string;
  ppid: number;
  pgid: number;
  sid: number;
  /** Field 22, or undefined when the line carries none. */
  startTime: number | undefined;
}

function parseStat(text: string): StatFacts | undefined {
  // `pid (comm) state ppid pgrp session …` — comm may itself contain ")" and spaces.
  const open = text.indexOf("(");
  const close = text.lastIndexOf(")");
  if (open < 0 || close < open) return undefined;
  const comm = sanitizeForLog(text.slice(open + 1, close), 64);
  const fields = text.slice(close + 2).trim().split(/\s+/);
  const ppid = Number(fields[1]);
  const pgid = Number(fields[2]);
  const sid = Number(fields[3]);
  if (![ppid, pgid, sid].every(Number.isInteger)) return undefined;
  return { comm, ppid, pgid, sid, startTime: statStartTime(text) };
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
export interface MaintenanceBootstrapRoot extends RecordedRoot {
  parentPid: number;
  executable: string;
}

/** One synchronous snapshot at the FIRST statement of main, before any app
 * spawn or claim. No later enrollment. Parameters are pure-table test seams;
 * production calls this with defaults. Unknown bootstrap state grants nothing. */
export function snapshotMaintenanceBootstrap(
  table: ProcTable = procfsTableAt(PROC_ROOT, { deadline: Date.now() + 1_000 }),
  expectedExecutable: () => string = maintenanceEsbuildExecutable,
  workerPid: number = process.pid,
): readonly MaintenanceBootstrapRoot[] | undefined {
  if (process.platform !== "linux") return undefined;
  try {
    const pids = table.listPids();
    if (pids.length > 128 || new Set(pids).size !== pids.length ||
        pids.some(pid => !Number.isSafeInteger(pid) || pid <= 0)) return undefined;
    const children: Array<{ pid: number; startTime: number }> = [];
    for (const pid of pids) {
      const raw = table.readStat(pid), parsed = parseStat(raw);
      if (!raw.startsWith(`${pid} (`) || !parsed || parsed.startTime === undefined) return undefined;
      if (parsed.ppid === workerPid) children.push({ pid, startTime: parsed.startTime });
    }
    if (children.length === 0) return Object.freeze([]);
    if (children.length !== 1 || !table.readExecutable) return undefined;
    const expected = expectedExecutable();
    if (!path.isAbsolute(expected) || path.resolve(expected) !== expected || Buffer.byteLength(expected) > 4096) return undefined;
    const child = children[0]!, raw = table.readStat(child.pid), current = parseStat(raw);
    if (!raw.startsWith(`${child.pid} (`) || current?.ppid !== workerPid || current.startTime !== child.startTime ||
        table.readExecutable(child.pid) !== expected) return undefined;
    return Object.freeze([Object.freeze({ ...child, parentPid: workerPid, executable: expected })]);
  } catch { return undefined; }
}

// The worker's one bootstrap snapshot. Recorded once by main; never re-taken or widened later.
let recordedBootstrap: { roots: readonly MaintenanceBootstrapRoot[] | undefined } | undefined;

/** Record main's first-statement snapshot. A second call refuses: no later enrollment (E3). */
export function recordMaintenanceBootstrap(roots: readonly MaintenanceBootstrapRoot[] | undefined): void {
  if (recordedBootstrap !== undefined) throw new Error("maintenance bootstrap already recorded");
  // A detached deep copy: neither the caller's array nor its entries can change the record.
  recordedBootstrap = Object.freeze({ roots: roots === undefined ? undefined : Object.freeze(roots.map(root =>
    Object.freeze({ pid: root.pid, startTime: root.startTime, parentPid: root.parentPid, executable: root.executable }))) });
}

/** The recorded exact bootstrap set, or undefined (unrecorded or refused): the producer fails closed. */
export function maintenanceBootstrapRoots(): readonly MaintenanceBootstrapRoot[] | undefined {
  return recordedBootstrap?.roots;
}

/** The #3031 writer-exclusion proof: the worker-local, deny-only, never-signalling process scan,
 * then (when Docker is wired) the read-only running-container probe, both inside ONE accounting
 * scope. One pass inside uncharged discovery (C2 pre-check), three inside the charged slot.
 * Without a recorded bootstrap set it refuses. */
export function terminalMaintenanceProof(scans: 1 | 3, dockerHost: string | undefined): Promise<boolean> {
  const bootstrap = maintenanceBootstrapRoots();
  if (bootstrap === undefined) return Promise.resolve(false);
  const req: ScanRequest = {
    mode: "capture", targetUid: process.getuid?.() ?? 0, targetKey: "", targetPaths: [], liveMarkers: [],
    liveRoots: [], workerNonce: "", exclusiveIdle: true, exclusiveIdleScans: scans,
    exclusiveIdleRoots: [], exclusiveIdleBootstrap: [...bootstrap],
  };
  const proof = async () => (await reapRunProcesses(req, { viaHelper: false })).state === "quiescent" &&
    (dockerHost === undefined || await dockerRunningContainers(dockerHost, MaintenanceResources.signalIfActive()) === "none");
  return scans === 1 ? MaintenanceResources.runDiscovery(proof) : proof();
}

/** Bounded, non-gating diagnostics (E6). A value it cannot read is null, never a guess. */
export function readMaintenancePosture(root: string = PROC_ROOT): {
  yamaPtraceScope: number | null; seccomp: number | null; capEff: string | null; uidSplit: boolean;
} {
  const small = (file: string): string | null => {
    try {
      const fd = fs.openSync(path.join(root, file), fs.constants.O_RDONLY | fs.constants.O_NOFOLLOW | fs.constants.O_NONBLOCK);
      try {
        const buf = Buffer.alloc(64 * 1024 + 1);
        const length = fs.readSync(fd, buf, 0, buf.length, null);
        return length > 64 * 1024 ? null : buf.toString("utf8", 0, length);
      } finally { fs.closeSync(fd); }
    } catch { return null; }
  };
  const yama = small("sys/kernel/yama/ptrace_scope")?.trim();
  const status = small("self/status") ?? "";
  const field = (name: string) => new RegExp(`^${name}:\\s*(\\S+)$`, "m").exec(status)?.[1];
  const seccomp = field("Seccomp"), capEff = field("CapEff");
  return {
    yamaPtraceScope: yama !== undefined && /^[0-3]$/.test(yama) ? Number(yama) : null,
    seccomp: seccomp !== undefined && /^[0-2]$/.test(seccomp) ? Number(seccomp) : null,
    capEff: capEff !== undefined && /^[0-9a-f]{16}$/.test(capEff) ? capEff : null,
    uidSplit: uidSplitActive(),
  };
}

export interface ScanRequest {
  reportDeadline?: number;
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
  /** Explicit permission for solitary uid-split reaping; omission is false. */
  mayKillUnreadableUnattributed?: boolean;
  /** issue #2213: a WORKER-WIDE detection scan (the pre-fetch check). No process is in scope and
   *  none is ever signalled: the scan only reports the runner-uid processes whose env and cwd
   *  cannot be read and that nothing ties to another live attempt. */
  workerWide?: boolean;
  /** Deny-only terminal-maintenance proof. Unlike workerWide, every UID and
   * readable orphan is in scope. Never shares ordinary mark/group exemptions. */
  exclusiveIdle?: true;
  /** Fixed scan count for {@link exclusiveIdle}: 1 for the uncharged pre-check, at most 3 in a slot. */
  exclusiveIdleScans?: number;
  /** Exact, trusted bootstrap/slot roots supplied by the worker, never inferred
   * from a name, environment nonce, process group or current idle observation. */
  exclusiveIdleRoots?: RecordedRoot[];
  /** Bootstrap services are exact-only identities, never subtree exemptions. */
  exclusiveIdleBootstrap?: MaintenanceBootstrapRoot[];
  /** Recorded roots (pid + start time) of every OTHER live attempt on this worker (Claude CLI
   *  groups, Codex provider supervisors), plus the long-lived runner-uid roots the worker itself
   *  launched. A root matches only while a live process has BOTH its pid and its start time. */
  liveRoots: RecordedRoot[];
  /** This worker's spawn nonce: a process carrying it is never run-owned. */
  workerNonce: string;
  /** TEST ONLY: the {@link QuiescenceView} the helper reaps against. The worker adds it to the
   *  helper's stdin request only from the view a test installed with
   *  {@link setQuiescenceViewForTests} (never from the environment), and the helper honours whatever
   *  `view` that stdin request carries: the stdin is the worker's own pipe to its child. With no view
   *  the helper scans the real procfs. */
  view?: QuiescenceView;
}

/** One listed process in a result. */
export interface QuiesceProcess {
  pid: number;
  uid: number;
  comm: string;
  cwd: string;
  reason: string;
}

interface KillCandidate extends QuiesceProcess {
  /** Internal identity captured during classification; never included in ProcessQuiescence. */
  startTime: number | undefined;
}

interface ScanResult {
  kill: KillCandidate[];
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

/** Recorded roots by pid → the start times recorded for that pid. */
type RootIndex = ReadonlyMap<number, ReadonlySet<number>>;

function indexRoots(roots: readonly RecordedRoot[]): RootIndex {
  const out = new Map<number, Set<number>>();
  for (const root of roots) {
    const times = out.get(root.pid) ?? new Set<number>();
    times.add(root.startTime);
    out.set(root.pid, times);
  }
  return out;
}

/** True when `candidate` is a live process that IS a recorded root: its pid is recorded AND its
 *  current start time equals the one recorded. A recycled pid (same number, a later start time)
 *  never matches, and neither does a root that is gone (its start time cannot be read). */
function isRecordedRoot(candidate: number, table: ProcTable, roots: RootIndex): boolean {
  const recorded = roots.get(candidate);
  if (recorded === undefined) return false;
  const r = tryRead(() => table.readStat(candidate));
  const startTime = r.ok ? parseStat(r.value)?.startTime : undefined;
  return startTime !== undefined && recorded.has(startTime);
}

/** True when an unreadable pid IS, or leads by ppid chain, process group or session to, a recorded
 *  root of another live attempt or a worker-launched runner-uid root (pid AND start time). */
function attributedToOtherLive(pid: number, table: ProcTable, roots: RootIndex): boolean {
  if (roots.size === 0) return false;
  const candidates = new Set<number>([pid]);
  const r = tryRead(() => table.readStat(pid));
  if (r.ok) {
    const facts = parseStat(r.value);
    if (facts) {
      candidates.add(facts.pgid);
      candidates.add(facts.sid);
    }
  }
  for (const a of ancestorsOf(pid, table)) candidates.add(a);
  return [...candidates].some((c) => isRecordedRoot(c, table, roots));
}

/** One pass over the process table. Pure apart from the reads; signals nothing. */
export function scanOnce(
  req: ScanRequest,
  table: ProcTable,
  selfPid: number = process.pid,
  excludeAllDescendants = false,
): ScanResult {
  if (req.exclusiveIdle === true) return scanExclusiveIdle(req, table, selfPid, excludeAllDescendants);
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
  const startTimeOf = new Map<number, number>();
  for (const pid of pids) {
    const r = tryRead(() => table.readStat(pid));
    const facts = r.ok ? parseStat(r.value) : undefined;
    if (facts) {
      parentOf.set(pid, facts.ppid);
      if (facts.startTime !== undefined) startTimeOf.set(pid, facts.startTime);
    }
  }
  const isDescendant = (pid: number): boolean | undefined => {
    let cur = parentOf.get(pid);
    for (let depth = 0; depth < 256; depth++) {
      if (cur === undefined) return undefined;
      if (cur <= 1) return false;
      if (cur === selfPid) return true;
      cur = parentOf.get(cur);
    }
    return undefined;
  };
  const liveMarkers = new Set(req.liveMarkers);
  const roots = indexRoots(req.liveRoots);

  for (const pid of pids) {
    if (excluded.has(pid)) continue;
    const descendant = isDescendant(pid);
    // The runner-uid helper owns every child it starts, including non-dumpable loader
    // children whose environment cannot be inspected for an attempt marker.
    if (descendant === true && excludeAllDescendants) continue;
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
      // A non-dumpable runner-uid process: attribute by ancestry before considering a signal.
      if (attributedToOtherLive(pid, table, roots)) continue;
      const statRead = tryRead(() => table.readStat(pid));
      const facts = statRead.ok ? parseStat(statRead.value) : undefined;
      const entry: QuiesceProcess = {
        pid,
        uid: status.uid,
        comm: facts?.comm || status.comm,
        cwd: cwdRead.ok ? sanitizeForLog(cwdRead.value) : "unreadable",
        reason: "unreadable_unattributed",
      };
      // Under the uid split every runner-uid process is: (1) agent-controlled code of a
      // current or finished claim, including deliberately unmarked dependency installs,
      // provisioning builds, self-improve checks, stub git and runner-clone git that can run
      // configured code; (2) a recorded worker-launched root or descendant (including the
      // non-dumpable Codex supervisor); (3) a readable, worker-marked fixed-argv spawn that
      // runs no repo code; or (4) this R5-excluded scanner. Thus an unreadable process with
      // no recorded-root attribution and no other claim/attempt in flight is reapable residue,
      // even when it came from a finished run. A future worker spawn that can become
      // non-dumpable must be recorded as a root or this invariant breaks.
      if (
        req.workerWide !== true &&
        req.mayKillUnreadableUnattributed === true &&
        req.liveMarkers.length === 0 &&
        descendant === false &&
        facts?.startTime !== undefined
      ) {
        result.kill.push({ ...entry, reason: "unreadable_no_other_claim", startTime: facts.startTime });
      } else {
        result.unverified.push(entry);
      }
      continue;
    }
    const env = parseEnviron(envRead.value);
    // The worker mark exempts a process only when it carries NO attempt marker: a worker spawn
    // never has one (workerSpawnEnv strips it), so an attempt-marked process that also claims the
    // nonce is agent residue that copied it, and is classified like any other.
    if (env.get(WORKER_SPAWN_ENV) === req.workerNonce && env.get(RUN_ATTEMPT_ENV) === undefined) continue;
    // R5: a descendant is the scanner's own unless an attempt marker says it is agent residue.
    if (descendant === true && env.get(RUN_ATTEMPT_ENV) === undefined) continue;
    const cwd = cwdRead.value;
    // R3: the key alone never scopes a process (a foreign run can carry another key's
    // UZI_RUN_CLONE_KEY with no marker and wedge that key's every seed); only together with a
    // well-formed attempt marker, which is what every agent CLI spawn carries.
    const inScope =
      req.workerWide !== true &&
      ((env.get(RUN_CLONE_KEY_ENV) === req.targetKey && wellFormedAttemptMarker(env.get(RUN_ATTEMPT_ENV))) ||
        req.targetPaths.some((p) => isWithinPath(stripDeleted(cwd), p)));
    if (!inScope) continue;
    const entry = (reason: string): QuiesceProcess => ({
      pid,
      uid: status.uid,
      comm: status.comm,
      cwd: sanitizeForLog(cwd),
      reason,
    });
    const candidate = (reason: string): KillCandidate => ({ ...entry(reason), startTime: startTimeOf.get(pid) });
    const marker = env.get(RUN_ATTEMPT_ENV);
    if (marker !== undefined && marker === req.ownMarker) result.kill.push(candidate("own_attempt"));
    else if (marker !== undefined && liveMarkers.has(marker)) result.survivors.push(entry("live_attempt_conflict"));
    else if (marker !== undefined) result.kill.push(candidate("terminal_attempt"));
    else if (req.mode === "own") result.kill.push(candidate("unmarked_in_scope"));
    else result.survivors.push(entry("unattributed_in_scope"));
  }
  return result;
}

/** True only when mountinfo has exactly one complete proc mount at /proc with no hidepid restriction.
 * `subset=pid` still lists every process; any hidepid other than 0/off hides some. */
export function procMountShowsAllProcesses(mountinfo: string): boolean {
  // Exactly one mount at /proc, a complete proc row. Stacked mounts are ambiguous: visibility
  // follows mount-id/parent topology, not row order, so more than one refuses.
  let options: string | undefined, mounts = 0;
  for (const line of mountinfo.split("\n")) {
    if (line === "") continue;
    const parts = line.split(" - ");
    if (parts.length !== 2) return false;
    const left = parts[0]!.split(" "), right = parts[1]!.split(" ");
    if (left.length < 6 || left.some(field => field === "") || right.length !== 3 || right.some(field => field === "")) return false;
    if (left[4] !== PROC_ROOT) continue;
    if (++mounts > 1) return false;
    options = right[0] === "proc" ? right[2] : undefined;
  }
  if (options === undefined) return false;
  return options.split(",").every(option => !option.startsWith("hidepid=") ||
    option === "hidepid=0" || option === "hidepid=off");
}

/** Read-only exclusion proof for the new producer, independent of run attribution.
 * Root enrollment is a separate trusted-worker obligation. This scanner only
 * verifies current pid/start identities and actual parent chains. */
function scanExclusiveIdle(req: ScanRequest, table: ProcTable, selfPid: number, helper: boolean): ScanResult {
  const result: ScanResult = { kill: [], survivors: [], unverified: [] };
  const roots = req.exclusiveIdleRoots;
  const bootstrap = req.exclusiveIdleBootstrap ?? [];
  if (req.mode !== "capture" || req.workerWide === true || req.targetKey !== "" ||
      req.targetPaths.length !== 0 || req.ownMarker !== undefined || req.liveMarkers.length !== 0 ||
      req.liveRoots.length !== 0 || req.mayKillUnreadableUnattributed === true ||
      !Array.isArray(roots) || roots.length > 32 || roots.some(root => !root ||
        !Number.isSafeInteger(root.pid) || root.pid <= 0 || !Number.isSafeInteger(root.startTime) || root.startTime < 0) ||
      new Set(roots.map(root => root.pid)).size !== roots.length || !Array.isArray(bootstrap) || bootstrap.length > 1 ||
      bootstrap.some(root => !root || !Number.isSafeInteger(root.pid) || root.pid <= 0 ||
        !Number.isSafeInteger(root.startTime) || root.startTime < 0 || !Number.isSafeInteger(root.parentPid) || root.parentPid <= 0 ||
        typeof root.executable !== "string" || Buffer.byteLength(root.executable) > 4096 ||
        !path.isAbsolute(root.executable) || path.resolve(root.executable) !== root.executable ||
        roots.some(other => other.pid === root.pid))) {
    result.tableError = "exclusive idle request invalid";
    return result;
  }
  // The active helper envelope, else the one this table was bound to. A wrapper table is
  // not registered, so looking it up alone would leave per-read admin work uncharged.
  const work = helperWork.getStore() ?? helperMeteredProcTables.get(table);
  const readStat = (pid: number) => {
    work?.consume("adminEntries", 1);
    MaintenanceResources.consumeIfActive("adminEntries", 1);
    return table.readStat(pid);
  };
  const readStatus = (pid: number) => {
    work?.consume("adminEntries", 1);
    MaintenanceResources.consumeIfActive("adminEntries", 1);
    return table.readStatus(pid);
  };
  const readExecutable = (pid: number) => {
    work?.consume("adminEntries", 1);
    MaintenanceResources.consumeIfActive("adminEntries", 1);
    if (!table.readExecutable) throw new Error("exclusive idle executable unavailable");
    return table.readExecutable(pid);
  };
  const identities = new Map<number, number>();
  // Enrolled pids observed as zombies: no exe link remains, start and parent still bind.
  const exitedBootstrap = new Set<number>();
  const facts = (pid: number) => {
    const raw = readStat(pid), parsed = parseStat(raw);
    // Do not let a malformed or substituted stat file manufacture a parent.
    if (!raw.startsWith(`${pid} (`) || !parsed || parsed.startTime === undefined) throw new Error("exclusive idle identity unreadable");
    if (identities.has(pid) && identities.get(pid) !== parsed.startTime) throw new Error("exclusive idle identity changed");
    identities.set(pid, parsed.startTime);
    return parsed;
  };
  try {
    // A hidepid-restricted proc mount lists only some processes: never a proof of idle.
    if (!table.readProcMountInfo) throw new Error("exclusive idle proc mount unproven");
    work?.consume("adminEntries", 1);
    MaintenanceResources.consumeIfActive("adminEntries", 1);
    if (!procMountShowsAllProcesses(table.readProcMountInfo())) throw new Error("exclusive idle proc mount unproven");
    const pids = table.listPids();
    if (pids.length > 128 || new Set(pids).size !== pids.length ||
        pids.some(pid => !Number.isSafeInteger(pid) || pid <= 0)) throw new Error("exclusive idle PID inventory refused");
    const own = facts(selfPid);
    // Only the current scanner and its positively observed ancestors are
    // implicit exclusions. Other descendants of the worker do not qualify.
    const excluded = new Set<number>([selfPid]);
    let ancestor = own.ppid;
    for (let depth = 0; ancestor > 0; depth++) {
      if (depth >= 128 || excluded.has(ancestor)) throw new Error("exclusive idle ancestry refused");
      const observed = facts(ancestor);
      excluded.add(ancestor);
      ancestor = observed.ppid;
    }
    if ([...roots, ...bootstrap].some(root => excluded.has(root.pid))) throw new Error("exclusive idle root overlaps worker ancestry");
    const trusted = new Map(roots.map(root => [root.pid, root.startTime]));
    // In the fixed scanner helper only, its current kernel identity anchors its
    // own compiler/helper children. This never exempts worker-wide descendants.
    if (helper) trusted.set(selfPid, own.startTime!);
    const inTrustedSubtree = (pid: number): boolean => {
      const seen = new Set<number>();
      for (let depth = 0; pid > 0; depth++) {
        if (depth >= 128 || seen.has(pid)) throw new Error("exclusive idle ancestry refused");
        seen.add(pid);
        const observed = facts(pid);
        if (trusted.get(pid) === observed.startTime) return true;
        pid = observed.ppid;
      }
      return false;
    };
    for (const pid of pids) {
      if (excluded.has(pid)) continue;
      // Bracket status with a stable kernel start identity. In particular, an
      // old zombie status must never exempt a new live process reusing its PID.
      const identity = tryRead(() => facts(pid));
      if (!identity.ok) {
        if (!identity.vanished) result.unverified.push({ pid, uid: -1, comm: "?", cwd: "not_read", reason: "exclusive_idle_identity_unreadable" });
        continue;
      }
      const statusRead = tryRead(() => readStatus(pid));
      if (!statusRead.ok) {
        if (!statusRead.vanished) result.unverified.push({ pid, uid: -1, comm: "?", cwd: "not_read", reason: "exclusive_idle_status_unreadable" });
        continue;
      }
      const status = parseStatus(statusRead.value);
      if (!status) {
        result.unverified.push({ pid, uid: -1, comm: "?", cwd: "not_read", reason: "exclusive_idle_status_unparsable" });
        continue;
      }
      const boot = bootstrap.find(root => root.pid === pid);
      if (status.zombie) {
        const zombie = tryRead(() => facts(pid));
        if (!zombie.ok && !zombie.vanished) result.unverified.push({ pid, uid: status.uid, comm: status.comm, cwd: "not_read", reason: "exclusive_idle_identity_unreadable" });
        // A zombie cannot write; an enrolled pid still keeps its exact start and parent (E3).
        else if (zombie.ok && boot && (zombie.value.startTime !== boot.startTime || zombie.value.ppid !== boot.parentPid))
          throw new Error("exclusive idle bootstrap identity changed");
        else if (zombie.ok && boot) exitedBootstrap.add(pid);
        continue;
      }
      if (boot) {
        const current = facts(pid);
        if (current.startTime !== boot.startTime || current.ppid !== boot.parentPid || readExecutable(pid) !== boot.executable)
          throw new Error("exclusive idle bootstrap identity changed");
        continue;
      }
      const ancestry = tryRead(() => inTrustedSubtree(pid));
      if (!ancestry.ok) {
        if (!ancestry.vanished) result.unverified.push({ pid, uid: status.uid, comm: status.comm, cwd: "not_read", reason: "exclusive_idle_identity_unreadable" });
      } else if (!ancestry.value) {
        result.survivors.push({ pid, uid: status.uid, comm: status.comm, cwd: "not_read", reason: "exclusive_idle_writer_present" });
      }
    }
    if (result.survivors.length || result.unverified.length) return result;
    // A disappearing parent cannot hide a newly listed orphan. No positive
    // answer from partial inventory, PID reuse or an unclassified new child.
    const after = table.listPids();
    if (after.length > 128 || new Set(after).size !== after.length ||
        after.some(pid => !pids.includes(pid) || !identities.has(pid))) throw new Error("exclusive idle inventory changed");
    for (const pid of after) {
      const current = facts(pid), boot = bootstrap.find(root => root.pid === pid);
      if (boot && (current.startTime !== boot.startTime || current.ppid !== boot.parentPid ||
          (!exitedBootstrap.has(pid) && readExecutable(pid) !== boot.executable)))
        throw new Error("exclusive idle bootstrap identity changed");
    }
  } catch {
    result.tableError = "exclusive idle inventory incomplete";
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
  reportBudget?: { signal: AbortSignal; deadline: number };
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
  const ambient = MaintenanceBudget.current();
  const remaining = ambient?.cap(deps.deadlineMs ?? REAP_DEADLINE_MS);
  // A test view stands in for the real table only when the caller injected none.
  const view = deps.table === undefined && testView !== undefined ? viewReapDeps(testView) : undefined;
  const scoped = MaintenanceResources.active();
  const reportBudget: { signal?: AbortSignal; deadline: number } | undefined = deps.reportBudget ??
    (req.reportDeadline === undefined ? (scoped ? {
      signal: MaintenanceResources.signalIfActive(),
      deadline: Date.now() + MaintenanceResources.remainingMsIfActive(),
    } : undefined) : { deadline: req.reportDeadline });
  const baseTable = deps.table ?? (reportBudget && testView && "procRoot" in testView
    ? procfsTableAt(testView.procRoot, reportBudget) : view?.table) ??
    (reportBudget || helperWork.getStore() ? procfsTableAt(PROC_ROOT, reportBudget) : procfsTable);
  // procfsTableAt binds raw reservations at construction. Returned rows/strings cannot
  // account for ignored directory entries, EOF or prebuffer reservations in another context.
  const work = helperWork.getStore();
  if (work && helperMeteredProcTables.has(baseTable) && helperMeteredProcTables.get(baseTable) !== work)
    return { state: "unverified", processes: [], killed: [], detail: "proc table helper work context mismatch" };
  let tableFailure: unknown;
  let bytes = 0;
  const bounded = <T>(read: () => T, cap?: number): T => {
    try {
      ambient?.check();
      MaintenanceResources.checkIfActive();
      reportBudget?.signal?.throwIfAborted();
      if (reportBudget && Date.now() >= reportBudget.deadline) throw new Error("process proof deadline");
      const result = read();
      if (typeof result === "string") {
        const work = helperWork.getStore();
        if (work && helperMeteredProcTables.get(baseTable) !== work)
          work.consume("metadataBytes", Buffer.byteLength(result));
        if (scoped && !meteredProcTables.has(baseTable))
          MaintenanceResources.consumeIfActive("metadataBytes", Buffer.byteLength(result));
      }
      if ((reportBudget || scoped) && typeof result === "string" && cap !== undefined) {
        const length = Buffer.byteLength(result);
        bytes += length;
        if (length > cap || bytes > 32 * 1024 * 1024) throw new Error("process input budget");
      }
      return result;
    } catch (error) {
      // Vanishing processes retain the existing attribution semantics. Budget and
      // incomplete-table failures cannot be hidden by scanOnce's per-PID tryRead.
      if (!["ENOENT", "ESRCH"].includes((error as NodeJS.ErrnoException).code ?? "")) tableFailure = error;
      throw error;
    }
  };
  const table: ProcTable = reportBudget || ambient || scoped ? {
    listPids: () => bounded(() => {
      const pids = baseTable.listPids();
      const work = helperWork.getStore();
      if (work && helperMeteredProcTables.get(baseTable) !== work) work.consume("adminEntries", pids.length);
      if (!meteredProcTables.has(baseTable)) MaintenanceResources.consumeIfActive("adminEntries", pids.length);
      if (pids.length > 4096) throw new Error("process PID budget");
      return pids;
    }),
    readStatus: pid => bounded(() => baseTable.readStatus(pid), 64 * 1024),
    readStat: pid => bounded(() => baseTable.readStat(pid), 64 * 1024),
    readEnviron: pid => bounded(() => baseTable.readEnviron(pid), 256 * 1024),
    readCwd: pid => bounded(() => baseTable.readCwd(pid), 64 * 1024),
    ...(baseTable.readExecutable ? { readExecutable: (pid: number) => bounded(() => baseTable.readExecutable!(pid), 64 * 1024) } : {}),
    ...(baseTable.readProcMountInfo ? { readProcMountInfo: () => bounded(() => baseTable.readProcMountInfo!(), 256 * 1024) } : {}),
  } : baseTable;
  // Exclusive-idle: a fixed small scan count, never kills, no inherited reap deadline loop.
  const exclusiveScans = req.exclusiveIdle === true ? req.exclusiveIdleScans : undefined;
  if (req.exclusiveIdle === true && (exclusiveScans === undefined || !Number.isSafeInteger(exclusiveScans) ||
      exclusiveScans < 1 || exclusiveScans > EXCLUSIVE_IDLE_MAX_SCANS ||
      // Uncharged discovery gets exactly one pass; rescans belong to a charged slot (C2).
      (MaintenanceResources.discoveryActive() && exclusiveScans !== 1)))
    return { state: "unverified", processes: [], killed: [], detail: "exclusive idle scan count invalid" };
  let scans = 0;
  const kill = deps.kill ?? view?.kill ?? defaultKill;
  const sleep = deps.sleep ?? defaultSleep;
  const now = deps.now ?? Date.now;
  // Report observations never kill a writer after byte inspection.
  const deadline = reportBudget ? now() : now() + (deps.deadlineMs ?? view?.deadlineMs ?? REAP_DEADLINE_MS);
  const selfPid = deps.selfPid ?? process.pid;
  const killed: number[] = [];
  let last: ScanResult;
  for (;;) {
    if (ambient && (ambient.signal.aborted || ambient.remainingMs() <= 0)) {
      return { state: "unverified", processes: [], killed, detail: "maintenance deadline exhausted" };
    }
    MaintenanceResources.checkIfActive();
    last = scanOnce(req, table, selfPid, deps.excludeAllDescendants ?? false);
    if (last.tableError || tableFailure || helperWork.getStore()?.failed || (ambient && (ambient.signal.aborted || ambient.remainingMs() <= 0)) || (reportBudget && Date.now() >= reportBudget.deadline)) {
      return { state: "unverified", processes: [], killed, detail: last.tableError ?? String(tableFailure ?? (helperWork.getStore()?.failed ? "helper work exhausted" : "process proof deadline")) };
    }
    if (exclusiveScans !== undefined) {
      if (last.kill.length > 0) return { state: "unverified", processes: [], killed, detail: "exclusive idle scan proposed a signal" };
      // A transient worker helper may clear within the fixed count; nothing is ever signalled.
      if ((last.survivors.length === 0 && last.unverified.length === 0) || ++scans >= exclusiveScans ||
          (reportBudget ? Date.now() >= reportBudget.deadline : remaining ? remaining() <= 0 : false)) break;
      if (ambient) await ambient.raceIO(() => sleep(Math.min(EXCLUSIVE_IDLE_INTERVAL_MS, remaining!())));
      else await sleep(EXCLUSIVE_IDLE_INTERVAL_MS);
      continue;
    }
    // An unreadable pid is rescanned too: a process caught mid-execve (or mid-exit) can read
    // back EACCES for an instant. Only one that stays unattributable to the deadline counts.
    if ((last.kill.length === 0 && last.unverified.length === 0) || (reportBudget ? now() >= deadline : remaining ? remaining() <= 0 : now() >= deadline)) break;
    for (const p of last.kill) {
      const statRead = tryRead(() => table.readStat(p.pid));
      const currentStartTime = statRead.ok ? parseStat(statRead.value)?.startTime : undefined;
      if (p.startTime === undefined || currentStartTime !== p.startTime) continue;
      ambient?.check();
      if (remaining && remaining() <= 0) break;
      kill(p.pid);
      killed.push(p.pid);
    }
    const delay = deps.intervalMs ?? view?.intervalMs ?? REAP_INTERVAL_MS;
    if (ambient) await ambient.raceIO(() => sleep(Math.min(delay, remaining!())));
    else await sleep(delay);
  }
  const stuck: QuiesceProcess[] = last.kill.map((p) => ({
    pid: p.pid,
    uid: p.uid,
    comm: p.comm,
    cwd: p.cwd,
    reason: `${p.reason}:kill_unconfirmed`,
  }));
  if (last.unverified.length > 0) {
    return {
      state: "unverified",
      processes: [...last.unverified, ...last.survivors, ...stuck],
      killed,
      detail: unverifiedDetail(last.unverified),
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

/** How many unverified pids an {@link unverifiedDetail} names before "+N more". */
const UNVERIFIED_DETAIL_NAMED = 2;

/** Why an unverified process could not be attributed, in an operator's words. */
function unverifiedWhy(reason: string): string {
  switch (reason) {
    case "unreadable_unattributed":
      return "env/cwd unreadable";
    case "status_unreadable":
      return "status unreadable";
    case "status_unparsable":
      return "status unparsable";
    default:
      return sanitizeForLog(reason, 64);
  }
}

/**
 * A process-chosen comm rendered as a double-quoted string for {@link unverifiedDetail}: sanitized
 * (control and bidi code points become `?`, capped at 64 characters plus `...`), then `"` and `\`
 * backslash-escaped, so no comm can close its own quotes and add text that reads as another
 * `pid N` entry. That holds for the FIRST entry, which always ends within the 160-character cap;
 * a later entry may be cut mid-quote by that cap, which a real (15-byte) comm never reaches. At most 2 + 2 x 64 + 3 = 133 characters (every character a `"`).
 */
function quotedComm(comm: string): string {
  let out = "";
  for (const ch of sanitizeForLog(comm, 64)) out += ch === '"' || ch === "\\" ? `\\${ch}` : ch;
  return `"${out}"`;
}

/**
 * issue #1783 M2 — the detail of an `unverified` verdict, naming the first pid and its comm FIRST
 * so they survive the 160-character cap RunResidueBlockedError / CloneResidueBlockedError put on
 * the failure_reason: an operator reads which process to kill. The first entry's pid and comm end
 * at most 156 characters in (`runner-uid pid ` 15, a 7-digit pid at Linux's pid_max 4194304, a
 * space, a {@link quotedComm} of at most 133). The comm is quoted because the process chooses it
 * and `(`, `)` and `;` are this detail's own separators. Up to {@link UNVERIFIED_DETAIL_NAMED} pids
 * are named, the rest counted as "+N more": the unreadable, unattributed ones first (they carry a
 * comm), then by pid, so the same table always reads back the same detail.
 */
function unverifiedDetail(unverified: readonly QuiesceProcess[]): string {
  const rank = (p: QuiesceProcess): number => (p.reason === "unreadable_unattributed" ? 0 : 1);
  const [first, ...rest] = [...unverified].sort((a, b) => rank(a) - rank(b) || a.pid - b.pid);
  if (first === undefined) return "no runner-uid process could be attributed";
  const name = (p: QuiesceProcess): string => `pid ${p.pid} ${quotedComm(p.comm)}`;
  let out = `runner-uid ${name(first)} could not be attributed (${unverifiedWhy(first.reason)})`;
  const named = rest.slice(0, UNVERIFIED_DETAIL_NAMED - 1);
  for (const p of named) out += `; ${name(p)} (${unverifiedWhy(p.reason)})`;
  const more = rest.length - named.length;
  if (more > 0) out += `; +${more} more`;
  return out;
}

/**
 * issue #1783 M2 review (N6) — true when a non-quiescent verdict is `unverified` SOLELY because of
 * runner-uid processes whose env and cwd could not be read and that ancestry attributes to nothing
 * (`unreadable_unattributed`, a non-dumpable process that may be anywhere on the worker): no
 * positively in-scope survivor, no live-owner conflict, no unconfirmed kill, no unreadable status,
 * no process-table error (an empty process list never qualifies). Only a SEED-mode caller may act on
 * it, and only to seed a FRESH path, which moves or frees nothing; see the runner's attempt seed.
 */
export function unverifiedOnlyByUnattributedUnreadable(q: ProcessQuiescence | undefined): boolean {
  return (
    q !== undefined &&
    q.state === "unverified" &&
    q.processes.length > 0 &&
    q.processes.every((p) => p.reason === "unreadable_unattributed")
  );
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

const defaultHelperSpawn: HelperSpawn = (command, args, opts) => {
  if (uidSplitActive() && hasReaperCaptureScope()) throw new Error("raw scoped helper refused");
  return spawn(command, args, { cwd: opts.cwd, env: opts.env, detached: true, stdio: ["pipe", "pipe", "pipe"] });
};

/** The helper's private TMPDIR: made fresh (mode 0700, owned by the uid the helper runs as)
 *  before every helper start and removed after it. Injectable for tests; either method may be
 *  synchronous or return a promise. */
export interface HelperTmp {
  make(budget?: { signal: AbortSignal; deadline: number }): string | Promise<string>;
  remove(dir: string, budget?: { signal: AbortSignal; deadline: number }): void | Promise<void>;
}

/** How a helper-TMPDIR command (mktemp / rm) is wrapped to run as the runner: `runnerCommand`
 *  in production, injectable so a test can drive the uid-split branch on a single-uid host. */
type RunnerWrap = (command: string, args: string[]) => { command: string; args: string[] };

const HELPER_TMP_MAKE_TIMEOUT_MS = 5_000;
const HELPER_TMP_REMOVE_TIMEOUT_MS = 10_000;

/**
 * Run one fixed, worker-authored command (as the runner, through `wrap`) ASYNCHRONOUSLY, with the
 * worker mark, stdout captured (bounded) and a hard timeout: a hung mktemp or rm never blocks the
 * worker's event loop (a spawnSync would, for up to its whole timeout, stalling every other run's
 * heartbeat and sink) and never outlives its deadline (the group is killed and the call rejects).
 */
function runFixedAsRunner(
  wrap: RunnerWrap,
  command: string,
  args: string[],
  timeoutMs: number,
  budget?: { signal: AbortSignal; deadline: number },
): Promise<{ status: number | null; stdout: string }> {
  if (uidSplitActive() && hasReaperCaptureScope()) return Promise.reject(new Error("raw scoped helper prelude refused"));
  const ambient = MaintenanceBudget.current();
  const scoped = MaintenanceResources.active();
  const resourceSignal = MaintenanceResources.signalIfActive();
  const within = AsyncLocalStorage.snapshot();
  ambient?.check();
  MaintenanceResources.checkIfActive();
  const remaining = ambient?.cap(timeoutMs);
  if (budget) {
    budget.signal.throwIfAborted();
    if (Date.now() >= budget.deadline) return Promise.reject(new Error("report helper setup deadline"));
  }
  const w = wrap(command, args);
  ambient?.check();
  const duration = remaining ? Math.floor(remaining()) : timeoutMs;
  if (remaining && duration <= 0) return Promise.reject(new Error("maintenance helper operation expired"));
  return new Promise((resolve, reject) => {
    let settled = false;
    MaintenanceResources.consumeIfActive("launches", 1);
    const child = spawn(w.command, w.args, {
      env: workerSpawnEnv({ PATH: HELPER_PATH }),
      detached: true,
      stdio: ["ignore", "pipe", "ignore"],
    });
    let cancellation: Error | undefined;
    const cancel = () => {
      if (settled) return;
      cancellation = new Error("report helper setup cancelled");
      killRunnerGroup(child.pid);
    };
    resourceSignal?.addEventListener("abort", cancel, { once: true });
    ambient?.signal.addEventListener("abort", cancel, { once: true });
    budget?.signal.addEventListener("abort", cancel, { once: true });
    if (budget?.signal.aborted || ambient?.signal.aborted || resourceSignal?.aborted) cancel();
    const timer = setTimeout(() => {
      if (settled) return;
      killRunnerGroup(child.pid);
      if (budget || ambient || scoped) cancellation = new Error("report helper setup deadline");
      else {
        settled = true;
        reject(new Error(`${command} as the runner timed out after ${timeoutMs} ms`));
      }
    }, Math.min(duration, budget ? Math.max(1, Math.min(timeoutMs, budget.deadline - Date.now())) : timeoutMs));
    let out = "";
    const raw: Buffer[] = [];
    if (scoped) {
      pullMaintenance(child.stdout, 64 * 1024, chunk => { raw.push(chunk); }, cancel);
    } else {
      child.stdout?.setEncoding("utf8");
      child.stdout?.on("data", (c: string) => {
        if (budget && Buffer.byteLength(out) + Buffer.byteLength(c) > 64 * 1024) {
          cancellation = new Error("report helper setup output overflow");
          killRunnerGroup(child.pid);
          return;
        }
        if (out.length < 64 * 1024) out += c;
      });
    }
    child.on("error", (err) => {
      if (settled) return;
      if (budget || ambient || scoped) { cancellation = err; if (ambient || scoped) killRunnerGroup(child.pid); return; }
      settled = true;
      clearTimeout(timer);
      reject(err);
    });
    child.on("close", (status) => within(() => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      resourceSignal?.removeEventListener("abort", cancel);
      ambient?.signal.removeEventListener("abort", cancel);
      budget?.signal.removeEventListener("abort", cancel);
      try {
        ambient?.check();
        MaintenanceResources.checkIfActive();
        if (remaining && remaining() <= 0) throw new Error("maintenance helper operation expired");
      } catch (error) { cancellation = error as Error; }
      if (cancellation) reject(cancellation);
      else resolve({ status, stdout: scoped ? Buffer.concat(raw).toString("utf8") : out });
    }));
  });
}

/**
 * The uid-split HelperTmp: the private dir must be the RUNNER's, so `mktemp -d` (mode 0700) and
 * the final `rm -rf` run as the runner through `wrap`, each asynchronously and time-bounded (see
 * {@link runFixedAsRunner}). `make` rejects unless mktemp exited 0 with an absolute path strictly
 * inside `base`. Exported for its test (an injected `wrap` on a single-uid host).
 */
export function runnerHelperTmp(
  base: string,
  wrap: RunnerWrap = runnerCommand,
  timeouts: { makeMs?: number; removeMs?: number; budget?: { signal: AbortSignal; deadline: number } } = {},
): { make(): Promise<string>; remove(dir: string): Promise<void> } {
  return {
    make: async () => {
      const r = await runFixedAsRunner(
        wrap,
        "mktemp",
        ["-d", path.join(base, "uzi-quiesce-XXXXXXXXXX")],
        timeouts.makeMs ?? HELPER_TMP_MAKE_TIMEOUT_MS,
        timeouts.budget,
      );
      const dir = r.stdout.trim();
      if (r.status !== 0 || !path.isAbsolute(dir) || !isWithinPath(dir, base) || path.resolve(dir) === path.resolve(base)) {
        throw new Error(`mktemp as the runner exited ${String(r.status)}`);
      }
      return dir;
    },
    remove: async (dir) => {
      await runFixedAsRunner(wrap, "rm", ["-rf", "--", dir], timeouts.removeMs ?? HELPER_TMP_REMOVE_TIMEOUT_MS, timeouts.budget);
    },
  };
}

const defaultHelperTmp: HelperTmp = {
  make: budget => {
    const base = runnerTmpdir() ?? os.tmpdir();
    // Single-uid the helper runs as this process's uid, so a plain mkdtemp (mode 0700) is its own.
    if (!uidSplitActive()) return fs.promises.mkdtemp(path.join(base, "uzi-quiesce-"));
    return runnerHelperTmp(base, runnerCommand, { budget }).make();
  },
  remove: (dir, budget) => {
    if (!uidSplitActive()) return fs.promises.rm(dir, { recursive: true, force: true });
    return runnerHelperTmp(runnerTmpdir() ?? os.tmpdir(), runnerCommand, { budget }).remove(dir);
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
  if (Array.isArray(o.processes)) MaintenanceResources.consumeIfActive("adminEntries", o.processes.length);
  if (Array.isArray(o.killed)) MaintenanceResources.consumeIfActive("adminEntries", o.killed.length);
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
  req: HelperScanRequest,
  opts: { producerWorkLimits?: HelperWorkLimits; spawnHelper?: HelperSpawn; timeoutMs?: number; helperTmp?: HelperTmp; reportBudget?: { signal: AbortSignal; deadline: number } } = {},
): Promise<ProcessQuiescence> {
  if (uidSplitActive() && hasReaperCaptureScope()) {
    return { state: "unverified", processes: [], killed: [], detail: "raw scoped helper refused" };
  }
  if (req.exclusiveIdle === true)
    return { state: "unverified", processes: [], killed: [], detail: "exclusive idle proof is worker-local" };
  validateProducerWorkLimits(opts.producerWorkLimits, req.mode, req.workerWide, req.exclusiveIdle);
  const limits = { ...(opts.producerWorkLimits ?? HELPER_WORK_LIMITS) };
  const ambient = MaintenanceBudget.current();
  const scoped = MaintenanceResources.active();
  const resourceSignal = MaintenanceResources.signalIfActive();
  const within = AsyncLocalStorage.snapshot();
  const remaining = ambient?.cap(opts.timeoutMs ?? HELPER_TIMEOUT_MS);
  if (scoped) req = { ...req, reportDeadline: Math.floor(Math.min(req.reportDeadline ?? Infinity,
    Date.now() + MaintenanceResources.remainingMsIfActive())) };
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
    ambient?.check();
    MaintenanceResources.checkIfActive();
    opts.reportBudget?.signal.throwIfAborted();
    if (opts.reportBudget && Date.now() >= opts.reportBudget.deadline) throw new Error("report helper setup deadline");
    tmpDir = await helperTmp.make(opts.reportBudget);
  } catch (err) {
    return unverified(`quiescence helper TMPDIR unavailable: ${(err as Error).message}`);
  }
  try {
    ambient?.check();
    MaintenanceResources.checkIfActive();
    if (remaining && Math.floor(remaining()) <= 0) throw new Error("helper setup cap expired");
  } catch { return unverified("maintenance helper setup expired"); }
  // Report-proof temporary cleanup retains its separate wall-clock deadline: an aborted proof signal
  // cannot finish rm. This budget grants no report authority, and runFixedAsRunner
  // still awaits CLOSE if its cleanup deadline fires after the command starts.
  const cleanupBudget = () => ({ signal: new AbortController().signal,
    deadline: Date.now() + HELPER_TMP_REMOVE_TIMEOUT_MS });
  const removeTmp = (): void => {
    // Best effort, not awaited (the verdict does not wait on cleanup): a leftover private dir is
    // harmless, and a synchronous throw or a rejection is swallowed alike.
    try {
      void Promise.resolve(helperTmp.remove(tmpDir)).catch(() => undefined);
    } catch {
      // swallowed, see above
    }
  };
  if (opts.reportBudget && (opts.reportBudget.signal.aborted || Date.now() >= opts.reportBudget.deadline)) {
    if (!ambient && !scoped) await Promise.resolve().then(() => helperTmp.remove(tmpDir, cleanupBudget())).catch(() => undefined);
    return unverified("report helper setup expired");
  }
  const wrapped = runnerCommand(process.execPath, argv);
  const helperEnv: NodeJS.ProcessEnv = { PATH: runnerPath() ?? HELPER_PATH, TMPDIR: tmpDir, TSX_DISABLE_CACHE: "1" };
  return new Promise<ProcessQuiescence>((resolve) => {
    let settled = false;
    let timer: NodeJS.Timeout | undefined;
    const done = (r: ProcessQuiescence): void => {
      if (settled) return;
      settled = true;
      if (timer) clearTimeout(timer);
      resourceSignal?.removeEventListener("abort", cancelReport);
      opts.reportBudget?.signal.removeEventListener("abort", cancelReport);
      ambient?.signal.removeEventListener("abort", cancelReport);
      if (ambient || scoped) {
        // Only admit private-temp cleanup while this attempt still has time; join it once admitted.
        try {
          ambient?.check();
          MaintenanceResources.checkIfActive();
          Promise.resolve(helperTmp.remove(tmpDir, opts.reportBudget)).then(
            () => { try { ambient?.check(); MaintenanceResources.checkIfActive(); resolve(r); } catch { resolve(unverified("maintenance helper cleanup expired")); } },
            () => resolve(unverified("maintenance helper cleanup failed")));
        } catch { resolve(unverified("maintenance helper expired")); }
      } else if (opts.reportBudget) {
        // Settlement includes the already-started cleanup; expiry never releases
        // source or journal boundaries while a helper command is still running.
        Promise.resolve().then(() => helperTmp.remove(tmpDir, cleanupBudget())).then(
          () => resolve(opts.reportBudget!.signal.aborted || Date.now() >= opts.reportBudget!.deadline
            ? unverified("report helper cleanup expired") : r),
          () => resolve(unverified("report helper cleanup failed")));
      } else { removeTmp(); resolve(r); }
    };
    let reportFailure: ProcessQuiescence | undefined;
    let child: import("node:child_process").ChildProcess;
    const cancelReport = () => {
      reportFailure = unverified("report helper cancelled");
      killRunnerGroup(child.pid);
    };
    try {
      ambient?.check();
      if (remaining && Math.floor(remaining()) <= 0) throw new Error("maintenance helper operation expired");
      if (scoped) {
        // Reserve the whole child envelope before dispatch, without refunds even on partial failure.
        if (req.exclusiveIdle === true && MaintenanceResources.discoveryActive()) {
          MaintenanceResources.consumeIfActive("adminEntries", limits.adminEntries);
          MaintenanceResources.consumeIfActive("metadataBytes", limits.metadataBytes);
        } else {
          MaintenanceResources.consume("adminEntries", limits.adminEntries);
          MaintenanceResources.consume("metadataBytes", limits.metadataBytes);
        }
        req = { ...req, workLimits: { ...limits } };
      }
      MaintenanceResources.consumeIfActive("launches", 1);
      child = (opts.spawnHelper ?? defaultHelperSpawn)(wrapped.command, wrapped.args, {
        cwd: "/",
        env: workerSpawnEnv(helperEnv),
      });
    } catch (err) {
      done(unverified(`quiescence helper spawn failed: ${(err as Error).message}`));
      return;
    }
    resourceSignal?.addEventListener("abort", cancelReport, { once: true });
    ambient?.signal.addEventListener("abort", cancelReport, { once: true });
    opts.reportBudget?.signal.addEventListener("abort", cancelReport, { once: true });
    if (opts.reportBudget?.signal.aborted || ambient?.signal.aborted || resourceSignal?.aborted) cancelReport();
    timer = setTimeout(() => {
      killRunnerGroup(child.pid);
      if (opts.reportBudget || ambient || scoped) reportFailure = unverified("quiescence helper timed out");
      else done(unverified("quiescence helper timed out"));
    }, Math.min(remaining ? Math.max(1, remaining()) : opts.timeoutMs ?? HELPER_TIMEOUT_MS, opts.reportBudget ? Math.max(1, Math.min(opts.timeoutMs ?? HELPER_TIMEOUT_MS,
      opts.reportBudget.deadline - Date.now())) : opts.timeoutMs ?? HELPER_TIMEOUT_MS));
    const out: Buffer[] = [];
    let outBytes = 0;
    if (scoped) {
      pullMaintenance(child.stdout, HELPER_STDOUT_MAX, c => { out.push(c); }, cancelReport);
      pullMaintenance(child.stderr, HELPER_STDOUT_MAX, () => {}, cancelReport);
    } else {
      child.stdout?.on("data", (c: Buffer) => {
        if (settled) return;
        outBytes += c.length;
        if (outBytes > HELPER_STDOUT_MAX) {
          killRunnerGroup(child.pid);
          if (opts.reportBudget || ambient) reportFailure = unverified("quiescence helper output exceeded 1 MiB");
          else done(unverified("quiescence helper output exceeded 1 MiB"));
          return;
        }
        out.push(c);
      });
      child.stderr?.resume();
    }
    // A helper that dies before reading its request fails this write (EPIPE); the close handler
    // below reports that exit, so the write error itself is only swallowed here.
    child.stdin?.on("error", () => undefined);
    child.on("error", (err) => {
      const failure = unverified(`quiescence helper spawn failed: ${err.message}`);
      if (opts.reportBudget || ambient || scoped) { reportFailure = failure; if (ambient || scoped) killRunnerGroup(child.pid); }
      else done(failure);
    });
    child.on("close", (code, signal) => within(() => {
      try {
        ambient?.check();
        MaintenanceResources.checkIfActive();
        if (remaining && remaining() <= 0) throw new Error("maintenance helper operation expired");
      } catch { reportFailure = unverified("maintenance helper expired"); }
      if (reportFailure) { done(reportFailure); return; }
      if (code !== 0) {
        done(unverified(`quiescence helper exited ${code ?? signal ?? "abnormally"}`));
        return;
      }
      try { done(parseHelperVerdict(Buffer.concat(out).toString("utf8"))); }
      catch { done(unverified("maintenance helper verdict exhausted")); }
    }));
    if (ambient || scoped) {
      try {
        ambient?.check();
        MaintenanceResources.checkIfActive();
        if (remaining && remaining() <= 0) throw new Error("helper input cap expired");
        MaintenanceResources.consumeIfActive("adminEntries", req.targetPaths.length + req.liveMarkers.length + req.liveRoots.length);
        const input = JSON.stringify(testView === undefined ? req : { ...req, view: testView });
        if (Buffer.byteLength(input) > HELPER_REQUEST_MAX) throw new Error("helper request overflow");
        MaintenanceResources.consumeIfActive("metadataBytes", Buffer.byteLength(input));
        child.stdin?.end(input);
      } catch { cancelReport(); }
    } else child.stdin?.end(JSON.stringify(testView === undefined ? req : { ...req, view: testView }));
  });
}

/** Options for {@link reapRunProcesses}. */
export interface ReapRunOptions extends ReapDeps {
  /** Charged capture only; positive limits at most 16 MiB metadata and 128 admin entries. */
  producerWorkLimits?: HelperWorkLimits;
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
  if (uidSplitActive() && hasReaperCaptureScope()) {
    return { state: "unverified", processes: [], killed: [], detail: "scoped reaper requires worker attempt construction" };
  }
  // The exclusive-idle proof never signals or reads environ/cwd, and its bootstrap
  // exe identity is readable only by the worker uid that owns it: always in-process.
  if (req.exclusiveIdle === true && opts.viaHelper === true)
    return { state: "unverified", processes: [], killed: [], detail: "exclusive idle proof is worker-local" };
  const viaHelper = req.exclusiveIdle === true ? false : opts.viaHelper ?? uidSplitActive();
  try {
    validateProducerWorkLimits(opts.producerWorkLimits, req.mode, req.workerWide, req.exclusiveIdle);
    if (viaHelper) {
    return await reapProcessesViaHelper(req, {
      spawnHelper: opts.spawnHelper,
      timeoutMs: opts.helperTimeoutMs,
      helperTmp: opts.helperTmp,
      reportBudget: opts.reportBudget,
      producerWorkLimits: opts.producerWorkLimits,
    });
  }
    // In-process work keeps actual scoped parent metering and a sticky local envelope.
    return await (opts.producerWorkLimits === undefined ? reapProcesses(req, opts) :
      helperWork.run(new HelperWork(opts.producerWorkLimits), () => reapProcesses(req, opts)));
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
  /** Clock in real-scale milliseconds (default `Date.now`). Differences between its readings
   *  become real timer durations (each request's absolute timeout), so an injected clock must
   *  tick in milliseconds, not in arbitrary units. */
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

/**
 * One Docker Engine request. `timeoutMs` bounds it TWICE: as the socket inactivity timeout, and as
 * an absolute wall-clock timer that destroys the request (and any response) when it fires, so a
 * daemon trickling a body byte by byte cannot hold the request open past it. The promise settles
 * exactly once; the absolute timer is cleared on settle and unref'd so it never holds the loop.
 */
function dockerRequest(
  ep: Endpoint,
  method: string,
  urlPath: string,
  timeoutMs: number,
  maxBodyBytes: number = DOCKER_BODY_MAX,
  signal?: AbortSignal,
): Promise<{ status: number; body: string }> {
  signal?.throwIfAborted();
  if (MaintenanceResources.active()) return maintenanceDockerRequest(ep, method, urlPath, timeoutMs, maxBodyBytes, signal);
  return new Promise((resolve, reject) => {
    let settled = false;
    let res: http.IncomingMessage | undefined;
    const finish = (err: Error | undefined, value?: { status: number; body: string }): void => {
      if (settled) return;
      settled = true;
      clearTimeout(absolute);
      signal?.removeEventListener("abort", cancel);
      if (err) reject(err);
      else resolve(value!);
    };
    const abort = (err: Error): void => {
      finish(err);
      res?.destroy();
      req.destroy();
    };
    const req = http.request({ ...ep, method, path: urlPath, timeout: timeoutMs }, (r) => {
      res = r;
      const chunks: Buffer[] = [];
      let bytes = 0;
      r.on("data", (c: Buffer) => {
        if (settled) return;
        bytes += c.length;
        if (bytes > maxBodyBytes) {
          abort(new Error(`docker ${method} response body exceeded ${maxBodyBytes} bytes`));
          return;
        }
        chunks.push(c);
      });
      r.on("end", () => finish(undefined, { status: r.statusCode ?? 0, body: Buffer.concat(chunks).toString("utf8") }));
      r.on("error", (err) => finish(err));
      r.on("aborted", () => finish(new Error(`docker ${method} ${urlPath} response aborted`)));
    });
    const cancel = () => abort(new Error("maintenance docker cancelled"));
    const absolute = setTimeout(
      () => abort(new Error(`docker ${method} ${urlPath} exceeded its ${timeoutMs}ms deadline`)),
      Math.max(0, timeoutMs),
    );
    absolute.unref();
    req.on("timeout", () => abort(new Error(`docker ${method} ${urlPath} timed out`)));
    req.on("error", (err) => finish(err));
    signal?.addEventListener("abort", cancel, { once: true });
    if (signal?.aborted) cancel();
    else req.end();
  });
}

/** Read-only running-container probe for the exclusive-idle proof: one GET of the
 * running list (never all=1, never a removal). Only an exact 200 empty array is "none". */
export async function dockerRunningContainers(dockerHost: string, signal?: AbortSignal): Promise<"none" | "running" | "unknown"> {
  const ep = parseDockerHost(dockerHost);
  if (!ep) return "unknown";
  try {
    const list = await dockerRequest(ep, "GET", "/containers/json", 5_000, 1024 * 1024, signal);
    if (list.status !== 200) return "unknown";
    const parsed: unknown = JSON.parse(list.body);
    if (!Array.isArray(parsed)) return "unknown";
    return parsed.length === 0 ? "none" : "running";
  } catch { return "unknown"; }
}

/** Scoped HTTP owns request and response through close, including quota-only cancellation. */
function maintenanceDockerRequest(ep: Endpoint, method: string, urlPath: string, timeoutMs: number,
  cap: number, signal?: AbortSignal): Promise<{ status: number; body: string }> {
  MaintenanceResources.checkIfActive();
  MaintenanceResources.consumeIfActive("rpcAttempts", 1);
  const duration = Math.max(0, Math.min(timeoutMs, MaintenanceResources.remainingMsIfActive()));
  const resourceSignal = MaintenanceResources.signalIfActive();
  const within = AsyncLocalStorage.snapshot();
  return new Promise((resolve, reject) => {
    let response: http.IncomingMessage | undefined;
    let requestClosed = false, responseClosed = false, ended = false, settled = false;
    let failure: Error | undefined;
    const chunks: Buffer[] = [];
    const finish = () => within(() => {
      if (settled || !requestClosed || (response && !responseClosed)) return;
      settled = true;
      clearTimeout(timer);
      signal?.removeEventListener("abort", cancel);
      resourceSignal?.removeEventListener("abort", cancel);
      try {
        MaintenanceResources.checkIfActive();
        if (failure) throw failure;
        if (!ended) throw new Error("maintenance docker incomplete");
        resolve({ status: response?.statusCode ?? 0, body: Buffer.concat(chunks).toString("utf8") });
      } catch (error) { reject(error); }
    });
    const abort = (error: Error) => {
      failure ??= error;
      response?.destroy();
      request.destroy();
    };
    const cancel = () => abort(new Error("maintenance docker cancelled"));
    const request = http.request({ ...ep, method, path: urlPath, timeout: timeoutMs }, r => within(() => {
      response = r;
      r.on("close", () => { responseClosed = true; finish(); });
      r.on("error", error => abort(error));
      r.on("aborted", cancel);
      r.on("end", () => { ended = true; });
      if (failure) { r.destroy(); return; }
      pullMaintenance(r, cap, chunk => chunks.push(chunk), cancel);
    }));
    const timer = setTimeout(cancel, duration);
    timer.unref();
    request.on("close", () => { requestClosed = true; finish(); });
    request.on("timeout", cancel);
    request.on("error", error => abort(error));
    signal?.addEventListener("abort", cancel, { once: true });
    resourceSignal?.addEventListener("abort", cancel, { once: true });
    try {
      MaintenanceResources.checkIfActive();
      if (signal?.aborted || resourceSignal?.aborted) cancel();
      else request.end();
    } catch { cancel(); }
  });
}

function boundContainers(body: string, targetPaths: string[]): string[] {
  const parsed: unknown = JSON.parse(body);
  if (!Array.isArray(parsed)) throw new Error("container listing is not an array");
  MaintenanceResources.consumeIfActive("adminEntries", parsed.length);
  const ids: string[] = [];
  for (const c of parsed) {
    if (typeof c !== "object" || c === null) continue;
    const id = (c as { Id?: unknown }).Id;
    const mounts = (c as { Mounts?: unknown }).Mounts;
    if (typeof id !== "string" || !Array.isArray(mounts)) continue;
    MaintenanceResources.consumeIfActive("adminEntries", mounts.length);
    const bound = mounts.some((m) => {
      const src = (m as { Source?: unknown } | null)?.Source;
      return typeof src === "string" && src !== "" && targetPaths.some((t) => isWithinPath(src, t));
    });
    if (bound) ids.push(id);
  }
  return ids;
}

/**
 * Force-remove every container with a mount source under a target path, together with its
 * anonymous volumes (`v=1`; named volumes survive, as Docker never removes them on container
 * removal), then keep listing (1 s apart) until two consecutive listings are clean. The overall deadline (15 s default) is checked
 * before EVERY request, list and delete alike: once it has passed no further request starts and
 * the result is `docker_error`. Each request is bounded by min(per-request timeout (5 s default),
 * time left to the deadline), applied both as the socket inactivity timeout and as an absolute
 * timer that destroys the request, so a daemon trickling a body cannot outlast it; the sleep
 * between listings is likewise capped at the time left and the deadline re-checked after it.
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
  const ambient = MaintenanceBudget.current();
  const deadline = now() + (opts.deadlineMs ?? 15_000);
  const localRemaining = ambient?.cap(opts.deadlineMs ?? 15_000) ?? (() => deadline - now());
  const remainingTime = () => Math.min(localRemaining(), MaintenanceResources.remainingMsIfActive());
  const interval = opts.intervalMs ?? 1_000;
  const removed = new Set<string>();
  const expired = (what: string): DockerTeardown => ({
    state: "docker_error",
    removed: [...removed],
    detail: `deadline passed before ${what}; stopped before two consecutive clean listings`,
  });
  let clean = 0;
  try {
    for (;;) {
      MaintenanceResources.checkIfActive();
      let remaining = remainingTime();
      if (remaining <= 0) return expired("the container listing");
      const list = await dockerRequest(
        ep,
        "GET",
        "/containers/json?all=1",
        Math.min(reqTimeout, remaining),
        opts.maxBodyBytes,
        ambient?.signal,
      );
      ambient?.check();
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
          MaintenanceResources.checkIfActive();
          remaining = remainingTime();
          if (remaining <= 0) return expired("a container deletion");
          const del = await dockerRequest(
            ep,
            "DELETE",
            `/containers/${encodeURIComponent(id)}?force=1&v=1`,
            Math.min(reqTimeout, remaining),
            opts.maxBodyBytes,
            ambient?.signal,
          );
          ambient?.check();
          if (del.status === 204 || del.status === 200 || del.status === 404) removed.add(sanitizeForLog(id, 128));
        }
      }
      remaining = remainingTime();
      if (remaining <= 0) return expired("the next listing");
      if (ambient) await ambient.raceIO(() => sleep(Math.min(interval, remaining)));
      else await sleep(Math.min(interval, remaining));
    }
  } catch (err) {
    return { state: "docker_error", removed: [...removed], detail: sanitizeForLog((err as Error).message) };
  }
}

// ─── Orchestration ─────────────────────────────────────────────────────────────────────────

/** One quiescence request for a run's clone. */
export interface QuiesceRunRequest {
  /** Producer retained-family capture only; ordinary boundaries omit this option. */
  producerWorkLimits?: HelperWorkLimits;
  reportBudget?: { signal: AbortSignal; deadline: number };
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
  /** issue #2213: a worker-wide detection scan (see ScanRequest.workerWide): nothing in scope,
   *  nothing signalled. The caller passes dockerHost undefined. */
  workerWide?: boolean;
  dockerHost: string | undefined;
  registry: LiveAttemptRegistry;
  /** Whether another claim is executing, including before it registers an attempt. */
  otherClaimInFlight?: boolean;
  /** The runner site asking (e.g. `finalize`, `park:after_runner_git` for a re-proof after a
   *  runner-clone git): diagnostic only, the proof itself never reads it. */
  site?: string;
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

/** Only quiesceRunAttempt's worker-owned construction enters this closed pipeline. */
function reapScopedRunProcesses(
  req: QuiesceRunRequest, scan: ScanRequest, deps: QuiesceRunDeps,
): Promise<ProcessQuiescence> {
  const attempt = req.attempt;
  if (!attempt || workerAttempts.get(attempt) !== JSON.stringify([attempt.marker, attempt.cloneKey, attempt.clonePath]) ||
      req.workerWide || req.cloneKey !== attempt.cloneKey ||
      req.targetPaths.length === 0 || req.targetPaths.some(p => ![attempt.clonePath, cloneKeyOf(attempt.clonePath).canonicalPath].includes(path.resolve(p))) ||
      testView !== undefined || Object.keys(deps).length !== 0) {
    return Promise.resolve({ state: "unverified", processes: [], killed: [], detail: "scoped reaper construction unavailable" });
  }
  // Snapshot caller data before the first await. No production view or test override
  // enters this request; its authority comes from the exact attempt and acquisition.
  const frozen = Object.freeze({ ...scan,
    targetPaths: Object.freeze([...scan.targetPaths]),
    liveMarkers: Object.freeze([...scan.liveMarkers]),
    liveRoots: Object.freeze(scan.liveRoots.map(root => Object.freeze({ ...root }))),
  });
  const reportBudget = req.reportBudget ? Object.freeze({ ...req.reportBudget }) : undefined;
  const helperDeadline = Date.now() + HELPER_TIMEOUT_MS;
  return runClosedReaper(attempt, async lane => {
    const ambient = MaintenanceBudget.current();
    const deadline = Math.floor(Math.min(lane.deadlineAt, reportBudget?.deadline ?? Infinity,
      helperDeadline, ambient ? Date.now() + ambient.remainingMs() : Infinity));
    if (!Number.isSafeInteger(deadline) || deadline <= Date.now()) throw new Error("scoped reaper deadline");
    const signal = AbortSignal.any([lane.signal, ...(reportBudget ? [reportBudget.signal] : []),
      ...(ambient ? [ambient.signal] : [])]);
    const check = () => {
      signal.throwIfAborted();
      ambient?.check();
      if (Date.now() >= deadline) throw new Error("scoped reaper deadline");
    };
    const input = JSON.stringify(reportBudget ? { ...frozen, reportDeadline: deadline } : frozen);
    if (Buffer.byteLength(input) > HELPER_REQUEST_MAX) throw new Error("scoped reaper input overflow");
    const base = runnerTmpdir() ?? os.tmpdir();
    const owner: ReaperTempOwner = { base, scanPath: runnerPath() ?? HELPER_PATH, removed: false };
    const execute = async (
      kind: "create" | "scan" | "remove",
    ): Promise<string> => {
      check();
      const dir = owner.created;
      const argv = kind === "create" ? ["/bin/mktemp", "-d", path.join(base, "uzi-quiesce-XXXXXXXXXX")]
        : kind === "remove" ? ["/bin/rm", "-rf", "--", dir!]
        : [process.execPath, ...helperArgv()];
      const env = Object.freeze(workerSpawnEnv(kind === "scan"
        ? { PATH: owner.scanPath, TMPDIR: dir!, TSX_DISABLE_CACHE: "1" }
        : { PATH: HELPER_PATH }));
      const request: BoundaryProcessRequest = Object.freeze({
        argv: Object.freeze(argv), cwd: "/", env, identity: "command",
        timeoutMs: Math.max(1, deadline - Date.now()),
      });
      const stdin = kind === "scan" ? input : "";
      const handle = await lane.spawn(request, stdin, deadline, owner, kind, signal);
      // The launcher writes the immutable bounded stdin before startup can complete.
      const chunks: Buffer[] = [];
      let bytes = 0;
      let outputFailure = false;
      let cancellation: Promise<void> | undefined;
      const cancel = () => {
        cancellation ??= handle.cancel();
        void cancellation.catch(() => undefined);
      };
      const cap = kind === "scan" ? HELPER_STDOUT_MAX : 64 * 1024;
      handle.stdout?.on("data", (chunk: Buffer | string) => {
        const buffer = typeof chunk === "string" ? Buffer.from(chunk) : chunk;
        bytes += buffer.length;
        if (bytes > cap) {
          outputFailure = true;
          cancel();
        } else chunks.push(buffer);
      });
      handle.stderr?.resume();
      const abort = cancel;
      signal.addEventListener("abort", abort, { once: true });
      if (signal.aborted) abort();
      let result: string | undefined;
      let primary: unknown;
      let failed = false;
      try {
        const terminal = await handle.completed;
        check();
        if (terminal.code !== 0 || outputFailure || !handle.stdout || !handle.stderr) throw new Error("scoped reaper operation failed");
        result = Buffer.concat(chunks).toString("utf8");
      } catch (error) {
        primary = error;
        failed = true;
      } finally {
        signal.removeEventListener("abort", abort);
      }
      try { await cancellation; }
      catch (cleanup) {
        throw new AggregateError(failed ? [primary, cleanup] : [cleanup], "reaper cancellation cleanup failed");
      }
      if (failed) throw primary;
      return result!;
    };
    let verdict: ProcessQuiescence;
    try {
      const output = await execute("create");
      const dir = output.trim();
      if (!path.isAbsolute(dir) || !isWithinPath(dir, base) || path.resolve(dir) === path.resolve(base) ||
          path.dirname(dir) !== path.resolve(base) || !/^uzi-quiesce-[A-Za-z0-9]+$/.test(path.basename(dir))) {
        throw new Error("scoped reaper temp invalid");
      }
      owner.created = dir;
      verdict = parseHelperVerdict(await execute("scan"));
    } catch {
      verdict = { state: "unverified", processes: [], killed: [], detail: "scoped reaper operation failed" };
    }
    // At most one exact-temp cleanup, under the same deadline. Expiry retains it;
    // every operation already admitted still joins through the safety owner.
    if (owner.created) {
      try { await execute("remove"); owner.removed = true; }
      catch { verdict = { ...verdict, state: "unverified", detail: `${verdict.detail ?? verdict.state}; scoped reaper private temp retained` }; }
    }
    return verdict;
  });
}

/** The process reap (unless skipped), then the Docker teardown. Never throws. */
export async function quiesceRunAttempt(req: QuiesceRunRequest, deps: QuiesceRunDeps = {}): Promise<QuiesceRunOutcome> {
  let processResult: ProcessQuiescence | undefined;
  const producerWorkLimits = req.producerWorkLimits === undefined ? deps.reap?.producerWorkLimits : req.producerWorkLimits;
  try {
    validateProducerWorkLimits(producerWorkLimits, req.mode, req.workerWide);
    if (producerWorkLimits !== undefined && (!req.processes || req.site !== "retained_ops"))
      throw new Error("invalid producer quiescence site");
  } catch (err) {
    return { process: { state: "unverified", processes: [], killed: [], detail: (err as Error).message },
      docker: { state: "not_wired", removed: [], detail: "" } };
  }
  const reapOptions = { ...deps.reap, producerWorkLimits };
  if (req.processes) {
    const others = req.registry.others(req.attempt?.marker);
    const scan: ScanRequest = {
      mode: req.mode,
      targetUid: deps.targetUid ?? defaultTargetUid(),
      targetKey: req.cloneKey,
      targetPaths: req.targetPaths.map((p) => path.resolve(p)),
      ownMarker: req.attempt?.marker,
      liveMarkers: others.map((a) => a.marker),
      mayKillUnreadableUnattributed:
        req.workerWide !== true && uidSplitActive() && req.otherClaimInFlight === false && others.length === 0,
      ...(req.workerWide === true ? { workerWide: true } : {}),
      // Other live attempts' recorded roots (Claude CLI groups, Codex provider supervisors) plus
      // every long-lived runner-uid root this worker launched and recorded itself.
      liveRoots: [
        ...others.flatMap((a) => {
          try {
            return a.recordedRootPids();
          } catch {
            return [];
          }
        }),
        ...workerRunnerRootPids(),
      ],
      workerNonce: workerSpawnNonce(),
    };
    if (uidSplitActive() && hasReaperCaptureScope()) {
      if (producerWorkLimits !== undefined) return {
        process: { state: "unverified", processes: [], killed: [], detail: "scoped producer work limits unsupported" },
        docker: { state: "not_wired", removed: [], detail: "" },
      };
      try {
        processResult = await reapScopedRunProcesses(req, scan, deps);
      } catch {
        processResult = { state: "unverified", processes: [], killed: [], detail: "scoped reaper refused or failed" };
      }
    } else processResult = await reapRunProcesses(req.reportBudget ? { ...scan, reportDeadline: req.reportBudget.deadline } : scan,
      req.reportBudget ? { ...reapOptions, reportBudget: req.reportBudget,
        deadlineMs: Math.max(0, req.reportBudget.deadline - Date.now()) } : reapOptions);
  }
  const docker = await teardownDocker({ ...deps.docker, dockerHost: req.dockerHost, targetPaths: req.targetPaths }).catch(
    (err: unknown): DockerTeardown => ({ state: "docker_error", removed: [], detail: (err as Error).message }),
  );
  const ambient = MaintenanceBudget.current();
  if (ambient && (ambient.signal.aborted || ambient.remainingMs() <= 0)) {
    processResult = { state: "unverified", processes: [], killed: processResult?.killed ?? [], detail: "maintenance deadline exhausted" };
  }
  return { process: processResult, docker };
}

// ─── Helper entry ──────────────────────────────────────────────────────────────────────────

function isHelperWorkLimits(v: unknown): v is HelperWorkLimits {
  if (typeof v !== "object" || v === null || Array.isArray(v)) return false;
  const o = v as Record<string, unknown>;
  return Object.keys(o).length === 2 && (Object.keys(HELPER_WORK_LIMITS) as (keyof HelperWorkLimits)[])
    .every(key => Object.hasOwn(o, key) && Number.isSafeInteger(o[key]) && (o[key] as number) >= 0 &&
      (o[key] as number) <= HELPER_WORK_LIMITS[key]);
}

function isScanRequest(v: unknown): v is HelperScanRequest {
  if (typeof v !== "object" || v === null) return false;
  const o = v as Record<string, unknown>;
  const strings = (x: unknown) => Array.isArray(x) && x.every((s) => typeof s === "string");
  return (
    (o.mode === "own" || o.mode === "seed" || o.mode === "capture") &&
    Number.isInteger(o.targetUid) &&
    typeof o.targetKey === "string" &&
    strings(o.targetPaths) &&
    // issue #2213: a worker-wide detection scan scopes nothing, so it carries no target paths.
    (o.workerWide !== true || (o.targetPaths as string[]).length === 0) &&
    (o.ownMarker === undefined || typeof o.ownMarker === "string") &&
    strings(o.liveMarkers) &&
    (o.mayKillUnreadableUnattributed === undefined || typeof o.mayKillUnreadableUnattributed === "boolean") &&
    (o.workerWide === undefined || typeof o.workerWide === "boolean") &&
    (o.exclusiveIdle === undefined || o.exclusiveIdle === true) &&
    (o.exclusiveIdleRoots === undefined || (Array.isArray(o.exclusiveIdleRoots) &&
      o.exclusiveIdleRoots.length <= 32 && o.exclusiveIdleRoots.every(r => r && typeof r === "object" &&
        Number.isSafeInteger((r as RecordedRoot).pid) && (r as RecordedRoot).pid > 0 &&
        Number.isSafeInteger((r as RecordedRoot).startTime) && (r as RecordedRoot).startTime >= 0))) &&
    (o.exclusiveIdle === true || o.exclusiveIdleRoots === undefined) &&
    (o.exclusiveIdleBootstrap === undefined || (Array.isArray(o.exclusiveIdleBootstrap) &&
      o.exclusiveIdleBootstrap.length <= 1 && o.exclusiveIdleBootstrap.every(r => r && typeof r === "object" &&
        Number.isSafeInteger((r as MaintenanceBootstrapRoot).pid) && (r as MaintenanceBootstrapRoot).pid > 0 &&
        Number.isSafeInteger((r as MaintenanceBootstrapRoot).startTime) && (r as MaintenanceBootstrapRoot).startTime >= 0 &&
        Number.isSafeInteger((r as MaintenanceBootstrapRoot).parentPid) && (r as MaintenanceBootstrapRoot).parentPid > 0 &&
        typeof (r as MaintenanceBootstrapRoot).executable === "string" &&
        Buffer.byteLength((r as MaintenanceBootstrapRoot).executable) <= 4096))) &&
    (o.exclusiveIdle === true || o.exclusiveIdleBootstrap === undefined) &&
    Array.isArray(o.liveRoots) &&
    o.liveRoots.every(
      (r) =>
        typeof r === "object" &&
        r !== null &&
        Number.isInteger((r as { pid?: unknown }).pid) &&
        Number.isSafeInteger((r as { startTime?: unknown }).startTime),
    ) &&
    typeof o.workerNonce === "string" &&
    (o.reportDeadline === undefined || (Number.isSafeInteger(o.reportDeadline) && (o.reportDeadline as number) > 0)) &&
    (o.workLimits === undefined || isHelperWorkLimits(o.workLimits)) &&
    (o.view === undefined || isQuiescenceView(o.view))
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
  const reap = () => {
    const view = req.view === undefined ? {} : viewReapDeps(req.view);
    return reapProcesses(req, { ...view, excludeAllDescendants: true });
  };
  return req.workLimits === undefined ? reap() : helperWork.run(new HelperWork(req.workLimits), reap);
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
    // issue #1783 M2: say plainly which entries are the operator's to identify and kill.
    ...(p.reason === "unreadable_unattributed" ? { attribution: "could not be attributed" } : {}),
  }));
}
