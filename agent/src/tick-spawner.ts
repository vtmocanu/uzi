// issue #1597 M2 — the process spawner the MID-TURN checkpoint tick scopes GitCache to
// (`git.withBoundaryProcessSpawner`), plus the lock-file custody that goes with cancelling it.
//
// The tick runs git (fetch-back into the worker bare, pack-objects, owner-stamp `git config`,
// runner-uid rev-parse of the clone) WHILE the agent's turn is alive, so it must be cancellable to
// the process: a quiescing run, a shutdown or a preempting sink waits for EVERY tick child to have
// exited before it touches the clone or the bare. Each child is spawned in its OWN process group
// (`detached: true`); on the tick signal the group gets SIGTERM, then SIGKILL after a grace, and a
// child's `completed` resolves only once the leader has exited. `settled()` resolves once every
// child spawned so far has exited.
//
// Lock custody. git removes its own `*.lock` files on SIGTERM, but a child that survives SIGTERM
// and is SIGKILLed leaves them behind, and a leftover lock in the worker bare would fail every
// later fetch-back / publish / finalize. We delete such a lock ONLY on PROVEN ownership: before
// each spawn the candidate bare lock paths are snapshotted (presence + dev/ino); just before the
// SIGKILL, `/proc/<pid>/fd/*` is read for every live member of the child's process group, recording
// open `*.lock` files inside the bare with their dev/ino. After exit a lock is removed iff it was
// ABSENT in the pre-spawn snapshot AND held open by the child AND is still the same dev/ino at that
// path. Everything else — non-Linux, a /proc read failure, an inode mismatch, a lock the child never
// opened (a foreign process), a lock that pre-existed the spawn — is RETAINED and reported with its
// evidence. Nothing is ever deleted by age.

import { spawn, spawnSync, type ChildProcess } from "node:child_process";
import fs from "node:fs/promises";
import path from "node:path";
import type { BoundaryProcessHandle, BoundaryProcessRequest } from "./harness.js";
import type { Logger } from "./log.js";
import { runnerCommand, uidSplitActive } from "./runner-uid.js";

/** SIGTERM → SIGKILL grace for a cancelled tick child. */
const DEFAULT_KILL_GRACE_MS = 2_000;

/** Presence + identity of one candidate lock path at a point in time. */
export interface LockStat {
  present: boolean;
  dev?: number;
  ino?: number;
}

/** A lock file a cancelled child held open, read from `/proc/<pid>/fd` just before its SIGKILL. */
interface HeldLock {
  path: string;
  dev: number;
  ino: number;
  pid: number;
}

/** A lock left behind that the spawner did NOT remove, with the evidence an operator needs. */
export interface RetainedLock {
  /** Absolute path of the lock file. */
  path: string;
  dev?: number;
  ino?: number;
  size?: number;
  mtimeMs?: number;
  /** What the pre-spawn snapshot recorded for this path (undefined = not a snapshot candidate). */
  preSpawn?: LockStat;
  /** Why it was kept: `foreign` (never held open by the child), `preexisting`, `inode_mismatch`,
   *  `no_proc_evidence` (/proc unreadable / not Linux), `not_snapshotted`, `unlink_failed`. */
  reason: string;
  /** The child's command class, e.g. `git fetch` — never its full argv (no paths, no credentials). */
  argvClass: string;
}

export interface LockReconcileResult {
  removed: string[];
  retained: RetainedLock[];
}

/** Test-only seams. Production passes none. */
export interface TickSpawnerTestHooks {
  /** Rewrite a request before it is spawned (e.g. swap a `git fetch` for a SIGTERM-ignoring
   *  sleeper). The rewritten argv[0] must still be absolute. */
  rewrite?: (request: BoundaryProcessRequest) => BoundaryProcessRequest;
  /** Observe every spawned child (pid + the ORIGINAL argv). */
  onSpawn?: (pid: number, argv: readonly string[]) => void;
  /** Runs at the start of a reconcile that has work to do (a cancelled child to reconcile), after
   *  every such child exited — lets a test replace a lock file between exit and the re-check. */
  beforeReconcile?: () => Promise<void>;
}

export interface TickSpawnerOptions {
  /** The tick's cancellation signal: every live child is terminated when it aborts, and no new
   *  child can be spawned after it. */
  signal: AbortSignal;
  /** The worker bare the tick mutates; its candidate lock paths are snapshotted per spawn. */
  barePath?: string;
  /** The run branch (for the nested `refs/uzi-runner/<branch>.lock` candidate). */
  branch?: string;
  log?: Logger;
  killGraceMs?: number;
  hooks?: TickSpawnerTestHooks;
}

interface Tracked {
  pid: number;
  argvClass: string;
  identity: BoundaryProcessRequest["identity"];
  exited: Promise<void>;
  isExited: () => boolean;
  snapshot: Map<string, LockStat> | undefined;
  /** Set once the child was signalled because of a cancellation. */
  signalled: boolean;
  /** Set when SIGKILL was needed; `held` is the /proc evidence (undefined = unreadable). */
  killed: boolean;
  held: HeldLock[] | undefined;
  reconciled: boolean;
}

/** `git -C <dir> -c k=v fetch …` → `git fetch`; anything else → its executable basename. */
export function argvClass(argv: readonly string[]): string {
  const exe = path.basename(argv[0] ?? "");
  if (exe !== "git") return exe;
  for (let i = 1; i < argv.length; i++) {
    const a = argv[i]!;
    if (a === "-C" || a === "-c") {
      i++;
      continue;
    }
    if (a.startsWith("-")) continue;
    return `git ${a}`;
  }
  return "git";
}

function abortError(): Error {
  const e = new Error("mid-turn checkpoint tick cancelled");
  e.name = "AbortError";
  return e;
}

async function statLock(p: string): Promise<LockStat> {
  try {
    const st = await fs.stat(p);
    return { present: true, dev: st.dev, ino: st.ino };
  } catch {
    return { present: false };
  }
}

/** The candidate lock paths inside a worker bare the tick's children could leave behind. */
async function candidateLockPaths(barePath: string, branch: string | undefined): Promise<string[]> {
  const out = [
    path.join(barePath, "packed-refs.lock"),
    path.join(barePath, "config.lock"),
    path.join(barePath, "shallow.lock"),
  ];
  // `refs/uzi-runner/<branch>.lock` — a branch with slashes nests (agent/issue-7 →
  // refs/uzi-runner/agent/issue-7.lock), which path.join reproduces.
  if (branch) out.push(path.join(barePath, "refs", "uzi-runner", `${branch}.lock`));
  try {
    for (const name of await fs.readdir(path.join(barePath, "objects", "info"))) {
      if (name.endsWith(".lock")) out.push(path.join(barePath, "objects", "info", name));
    }
  } catch {
    /* absent objects/info: no candidates there */
  }
  return out;
}

/** Every pid whose process group is `pgid` (Linux /proc). Throws when /proc is unreadable. */
async function groupMembers(pgid: number): Promise<number[]> {
  const pids: number[] = [];
  for (const name of await fs.readdir("/proc")) {
    if (!/^\d+$/.test(name)) continue;
    try {
      const stat = await fs.readFile(`/proc/${name}/stat`, "utf8");
      // Fields after the parenthesised comm: state ppid pgrp …
      const rest = stat.slice(stat.lastIndexOf(")") + 2).split(" ");
      if (Number(rest[2]) === pgid) pids.push(Number(name));
    } catch {
      /* raced exit */
    }
  }
  return pids;
}

/**
 * A {@link BoundaryProcessRequest}-compatible spawner for one mid-turn checkpoint tick. Identity
 * `command` reproduces what `runGitAsRunner` does OUTSIDE a boundary scope (inside one GitCache
 * skips the `runnerCommand` wrapper, so the spawner applies it); `worker_pat` spawns directly as
 * the worker. argv[0] must be absolute (GitCache resolves it via resolveBoundaryExecutable).
 */
export class TickSpawner {
  private readonly live = new Set<Tracked>();
  private readonly all: Tracked[] = [];
  private readonly killGraceMs: number;
  private bareReal: string | undefined;

  constructor(private readonly opts: TickSpawnerOptions) {
    this.killGraceMs = opts.killGraceMs ?? DEFAULT_KILL_GRACE_MS;
  }

  /** The spawner bound to the tick signal (hand this to `withBoundaryProcessSpawner`). */
  readonly spawn = (request: BoundaryProcessRequest): Promise<BoundaryProcessHandle> =>
    this.spawnWith(request, []);

  /** A spawner whose children ALSO die when `signal` aborts (a sub-deadline such as the 60s secret
   *  scan). Its children are tracked by, and settled with, this spawner. */
  scoped(signal: AbortSignal): (request: BoundaryProcessRequest) => Promise<BoundaryProcessHandle> {
    return (request) => this.spawnWith(request, [signal]);
  }

  /** Every pid spawned so far (for tests / evidence). */
  pids(): number[] {
    return this.all.map((t) => t.pid);
  }

  /** Resolves once every child spawned so far has exited. */
  async settled(): Promise<void> {
    while (this.live.size > 0) {
      await Promise.all([...this.live].map((t) => t.exited));
    }
  }

  /** True when any child had to be signalled by a cancellation (so lock reconcile is due). */
  cancelledAny(): boolean {
    return this.all.some((t) => t.signalled);
  }

  private async spawnWith(
    request: BoundaryProcessRequest,
    extraSignals: AbortSignal[],
  ): Promise<BoundaryProcessHandle> {
    const signals = [this.opts.signal, ...extraSignals];
    if (signals.some((s) => s.aborted)) throw abortError();
    if (!path.isAbsolute(request.argv[0] ?? "")) {
      throw new Error("tick subprocess executable is not a trusted absolute path");
    }
    const snapshot = this.opts.barePath ? await this.snapshotLocks() : undefined;
    // The snapshot is async: re-check so no child starts after a cancellation.
    if (signals.some((s) => s.aborted)) throw abortError();
    const req = this.opts.hooks?.rewrite ? this.opts.hooks.rewrite(request) : request;
    const [exe, ...args] = req.argv;
    // issue #1597 M2: identity `command` = the runner uid, exactly as runGitAsRunner applies it
    // outside a scope (setpriv under the uid split, a passthrough single-uid). worker_pat = this uid.
    const wrapped = req.identity === "command" ? runnerCommand(exe!, args) : { command: exe!, args };
    const child: ChildProcess = spawn(wrapped.command, wrapped.args, {
      cwd: req.cwd,
      env: req.env,
      detached: true,
      stdio: ["pipe", "pipe", "pipe"],
    });
    let exitedFlag = false;
    let resolveExit!: () => void;
    const exited = new Promise<void>((r) => (resolveExit = r));
    const tracked: Tracked = {
      pid: child.pid ?? -1,
      argvClass: argvClass(request.argv),
      identity: req.identity,
      exited,
      isExited: () => exitedFlag,
      snapshot,
      signalled: false,
      killed: false,
      held: undefined,
      reconciled: false,
    };
    const completed = new Promise<{ code: number }>((resolve, reject) => {
      child.once("error", (err) => {
        exitedFlag = true;
        this.live.delete(tracked);
        resolveExit();
        reject(err);
      });
      child.once("exit", (code, sig) => {
        exitedFlag = true;
        this.live.delete(tracked);
        resolveExit();
        resolve({ code: code ?? (sig ? 128 : 1) });
      });
    });
    completed.catch(() => undefined);
    if (child.pid === undefined) {
      // A spawn failure (ENOENT/EACCES): 'error' fires; nothing to track or kill.
      return { stdin: child.stdin, stdout: child.stdout, stderr: child.stderr, completed };
    }
    this.live.add(tracked);
    this.all.push(tracked);
    this.opts.hooks?.onSpawn?.(child.pid, request.argv);
    for (const s of signals) {
      s.addEventListener("abort", () => void this.terminate(tracked), { once: true });
    }
    return { stdin: child.stdin, stdout: child.stdout, stderr: child.stderr, completed };
  }

  private async snapshotLocks(): Promise<Map<string, LockStat>> {
    const snap = new Map<string, LockStat>();
    const bare = this.opts.barePath!;
    for (const p of await candidateLockPaths(bare, this.opts.branch)) snap.set(p, await statLock(p));
    return snap;
  }

  private signalGroup(t: Tracked, sig: "SIGTERM" | "SIGKILL"): void {
    if (t.identity === "command" && uidSplitActive()) {
      // The worker cannot signal a runner-uid process directly (EPERM): signal the group as the
      // runner uid, the same way killRunnerGroup reaps the agent tree.
      const w = runnerCommand("kill", [sig === "SIGTERM" ? "-TERM" : "-KILL", `-${t.pid}`]);
      spawnSync(w.command, w.args, { stdio: "ignore" });
      return;
    }
    try {
      process.kill(-t.pid, sig);
    } catch {
      try {
        process.kill(t.pid, sig);
      } catch {
        /* already gone */
      }
    }
  }

  private async terminate(t: Tracked): Promise<void> {
    if (t.isExited() || t.signalled) return;
    t.signalled = true;
    this.signalGroup(t, "SIGTERM");
    let timer: ReturnType<typeof setTimeout> | undefined;
    await Promise.race([
      t.exited,
      new Promise<void>((r) => {
        timer = setTimeout(r, this.killGraceMs);
      }),
    ]);
    clearTimeout(timer);
    if (t.isExited()) {
      // The leader is gone; reap any straggler still in its group (ESRCH is swallowed).
      this.signalGroup(t, "SIGKILL");
      return;
    }
    // Survived SIGTERM: record which bare lock files the group holds open BEFORE the SIGKILL, so
    // ownership can be PROVEN afterwards (the fds vanish with the process).
    t.killed = true;
    t.held = await this.readHeldLocks(t.pid);
    this.signalGroup(t, "SIGKILL");
    await t.exited;
  }

  private async readHeldLocks(pgid: number): Promise<HeldLock[] | undefined> {
    const bare = this.opts.barePath;
    if (!bare || process.platform !== "linux") return undefined;
    try {
      this.bareReal ??= await fs.realpath(bare);
      const held: HeldLock[] = [];
      const members = await groupMembers(pgid);
      if (!members.includes(pgid)) return undefined;
      for (const pid of members) {
        const fdDir = `/proc/${pid}/fd`;
        const fds = await fs.readdir(fdDir);
        for (const fd of fds) {
          let target: string;
          try {
            target = await fs.readlink(`${fdDir}/${fd}`);
          } catch {
            continue;
          }
          if (!target.endsWith(".lock")) continue;
          if (!target.startsWith(this.bareReal + path.sep)) continue;
          try {
            const st = await fs.stat(`${fdDir}/${fd}`);
            held.push({ path: target, dev: st.dev, ino: st.ino, pid });
          } catch {
            /* fd closed meanwhile */
          }
        }
      }
      return held;
    } catch (err) {
      this.opts.log?.warn("mid-turn checkpoint: could not read /proc for a cancelled child", {
        pid: pgid,
        error: err instanceof Error ? err.message : String(err),
      });
      return undefined;
    }
  }

  /**
   * Reconcile the bare's lock files after cancelled children exited. MUST be called while holding
   * the per-bare lock (GitCache.withLock / withBareLock) and after {@link settled}. Idempotent: each
   * child is reconciled once. Returns what was removed and what was retained (with evidence).
   */
  async reconcileLocks(): Promise<LockReconcileResult> {
    const result: LockReconcileResult = { removed: [], retained: [] };
    const bare = this.opts.barePath;
    if (!bare) return result;
    if (this.opts.hooks?.beforeReconcile && this.all.some((t) => !t.reconciled && t.signalled && t.isExited())) {
      await this.opts.hooks.beforeReconcile();
    }
    const seen = new Set<string>();
    for (const t of this.all) {
      if (t.reconciled || !t.signalled || !t.isExited()) continue;
      t.reconciled = true;
      const snapshot = t.snapshot ?? new Map<string, LockStat>();
      this.bareReal ??= await fs.realpath(bare).catch(() => bare);
      // Held paths are realpaths; snapshot keys are barePath-relative joins. Normalise to realpath.
      const toReal = (p: string): string =>
        this.bareReal && p.startsWith(bare) ? path.join(this.bareReal, p.slice(bare.length)) : p;
      const realSnapshot = new Map<string, LockStat>();
      for (const [p, v] of snapshot) realSnapshot.set(toReal(p), v);
      const nowCandidates = (await candidateLockPaths(bare, this.opts.branch)).map(toReal);
      const paths = new Set<string>([...nowCandidates, ...(t.held ?? []).map((h) => h.path)]);
      for (const p of paths) {
        if (seen.has(p)) continue;
        const now = await fs.stat(p).catch(() => undefined);
        if (!now) continue; // nothing left at this path
        seen.add(p);
        const pre = realSnapshot.get(p);
        const evidence = (reason: string): RetainedLock => ({
          path: p,
          dev: now.dev,
          ino: now.ino,
          size: now.size,
          mtimeMs: now.mtimeMs,
          preSpawn: pre,
          reason,
          argvClass: t.argvClass,
        });
        if (pre === undefined) {
          result.retained.push(evidence("not_snapshotted"));
          continue;
        }
        if (pre.present) {
          // It existed before this child started: not the child's, not a cancellation artefact.
          // Keep it, but it is only REPORTED when the child could have been blocked by it.
          result.retained.push(evidence("preexisting"));
          continue;
        }
        if (!t.killed) {
          // The child exited on SIGTERM (git cleans its own locks then): a new lock here is foreign.
          result.retained.push(evidence("foreign"));
          continue;
        }
        if (t.held === undefined) {
          result.retained.push(evidence("no_proc_evidence"));
          continue;
        }
        const held = t.held.find((h) => h.path === p);
        if (!held) {
          result.retained.push(evidence("foreign"));
          continue;
        }
        if (held.dev !== now.dev || held.ino !== now.ino) {
          result.retained.push(evidence("inode_mismatch"));
          continue;
        }
        try {
          await fs.unlink(p);
          result.removed.push(p);
        } catch {
          result.retained.push(evidence("unlink_failed"));
        }
      }
    }
    return result;
  }
}
