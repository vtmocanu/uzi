// issue #3404 M4 - the owning worker of the production-dispatch crash proofs
// (test/authority-crash-proofs.test.ts). A REAL process: it takes W with acquireWorkerLifetime, builds a
// REAL AuthorityRuntime and the production GitCache wiring (authority hooks + authorityRequired, exactly as
// main.ts buildWorkerGitCache), and drives one production operation inside an authority transaction. The
// test SIGKILLs it (never a cooperative exit) while a deterministic barrier holds the effect.
//
// The only injected pieces are the process-spawn seams the production code already exposes:
//  - scenario "delete": GitCacheOptions.retentionDelete.spawn replaces the detached `setpriv rm -rf` spawn
//    with a detached /bin/sh that waits on a FIFO and then runs the REAL `rm -rf -- <target>`;
//  - scenario "cleanup": GitCache's execScoped (the one place every git child is spawned) runs the
//    `update-ref -d` through a detached /bin/sh that waits on a FIFO and then execs the REAL
//    `git update-ref -d <ref> <sha>`. Everything else passes through untouched.
// Neither seam fakes the deletion or the ref update; the barrier only delays when the real child acts.
import { spawn } from "node:child_process";
import fs from "node:fs";
import fsp from "node:fs/promises";
import path from "node:path";
import { acquireWorkerLifetime, withAuthorityTransaction } from "../../src/authority-lock.js";
import { AuthorityRuntime } from "../../src/authority-runtime.js";
import type { AuthorityFsOps } from "../../src/completion-authority.js";
import { GitCache, type PositiveOwedCandidateContext } from "../../src/git.js";
import type { Logger } from "../../src/log.js";
import { armShutdownResultDeadline, finishWorkerShutdown, type ShutdownLog } from "../../src/shutdown-barrier.js";
import { testLockOptions } from "../authority-test-support.js";

export interface Barrier { armed: string; go: string; done: string; pid: string }
export interface ChildConfig {
  scenario: "delete" | "cleanup" | "late-queue" | "late-close-inline" | "pin-cleanup";
  dataDir: string;
  token: string;
  bare: string;
  /** null: the real child runs at once (the normally joined, fully accounted control). */
  barrier: Barrier | null;
  // scenario delete
  branch?: string;
  key?: string;
  runId?: string;
  // scenario cleanup
  context?: PositiveOwedCandidateContext;
  frozen?: Array<{ sha: string; contexts: PositiveOwedCandidateContext[] }>;
  coveragePins?: Array<{ fingerprint: string; sha: string }>;
  recoverySources?: string[];
  coveringSha?: string;
  /** which `update-ref -d` (1-based) the barrier holds */
  pauseAtDelete?: number;
  // issue #4066 shutdown-barrier scenarios (late-queue, late-close-inline, pin-cleanup). The SIGTERM handler mirrors
  // main.ts: arm the result observer, abort, join the scenario producer, await finishWorkerShutdown, report.
  /** late-queue: which late path the scenario exercises. */
  lateMode?: "plain" | "a1" | "a2" | "a3" | "b2";
  /** The second runner-uid delete's barrier (late-queue a1) and its clone. */
  barrier2?: Barrier;
  branch2?: string;
  key2?: string;
  runId2?: string;
  /** FIFO a paused hook (a1: after the late splice) or the raw A holder (a2) waits on. */
  holdGo?: string;
  /** FIFO the paused authority slot write waits on (late-close-inline) before its rename. */
  writeGo?: string;
  /** AuthorityRuntimeOptions.lateWaitMs. */
  lateWaitMs?: number;
  /** The shutdown budget, counted from the first signal (main.ts: SHUTDOWN_RESULT_DEADLINE_MS). */
  shutdownBudgetMs?: number;
  /** retentionDelete timeoutMs / closeWaitMs for the unsettled-delete scenarios. */
  rmTimeoutMs?: number;
  /** pin-cleanup: the archived recovery pin and the caller's deadline (ms from the call). */
  pin?: { runId: string; generation: number; sourceSha: string; deadlineMs: number };
  /** pin-cleanup: where the git wrapper script is written (put first on PATH). */
  wrapDir?: string;
}

const quiet: Logger = {
  debug() {}, info() {}, warn() {}, error() {}, addSecret() {}, removeSecret() {},
  child() { return quiet; },
};

const say = (line: string): void => { process.stdout.write(`${line}\n`); };

const forwardingLogger = (onMessage: (level: "info" | "warn", message: string, fields: Record<string, unknown> | undefined) => void): Logger => {
  const log: Logger = {
    debug() {}, error() {}, addSecret() {}, removeSecret() {},
    info(message: string, fields?: Record<string, unknown>) { onMessage("info", message, fields); },
    warn(message: string, fields?: Record<string, unknown>) { onMessage("warn", message, fields); },
    child() { return log; },
  } as Logger;
  return log;
};

/** A FIFO read: blocks until a writer opens and writes. The barrier is the FIFO, never a sleep. */
const waitFifo = async (fifo: string): Promise<void> => { await fsp.readFile(fifo); };
const openFifo = async (fifo: string): Promise<void> => { await fsp.writeFile(fifo, "go\n"); };

/** The real rm helper behind a FIFO (see scenario "delete"), one barrier per delete in order. */
function rmSpawner(rigs: Barrier[]): (command: string, args: string[]) => ReturnType<typeof spawn> {
  let n = 0;
  return (_command, args) => {
    const b = rigs[n++]!;
    const target = args.at(-1)!;
    return spawn("/bin/sh", ["-c",
      'echo $$ > "$4"; echo armed > "$1"; read _ < "$2"; /bin/rm -rf -- "$5"; echo $? > "$3"',
      "sh", b.armed, b.go, b.done, b.pid, target], { cwd: "/", detached: true, stdio: ["ignore", "ignore", "pipe"] });
  };
}

/**
 * issue #4066: the owning worker of the shutdown-barrier proofs. Same real AuthorityRuntime and GitCache wiring as the
 * crash proofs, plus a SIGTERM path that mirrors main.ts (observer armed at the first signal, scenario producer joined,
 * finishWorkerShutdown awaited, natural exit honoring process.exitCode). Stdout lines report the phases.
 */
async function runShutdownScenario(cfg: ChildConfig): Promise<void> {
  const lifetime = await acquireWorkerLifetime(cfg.dataDir, testLockOptions());
  const writeGate = { armed: false, faultRename: false, reached: undefined as (() => void) | undefined };
  const realRename = (from: string, to: string): Promise<void> => fsp.rename(from, to);
  const ops: AuthorityFsOps = {
    syncFile: (handle) => handle.sync(),
    syncDir: (handle) => handle.sync(),
    rename: async (from, to) => {
      if (writeGate.faultRename) { writeGate.faultRename = false; throw new Error("injected authority rename failure"); }
      if (writeGate.armed) {
        writeGate.armed = false;
        writeGate.reached?.();
        say("ARMED-WRITE");
        await waitFifo(cfg.writeGo!);
      }
      await realRename(from, to);
    },
  };
  let spliceHook = false;
  const rt = new AuthorityRuntime({
    lifetime, workerToken: cfg.token, timeoutMs: 20_000, ops,
    ...(cfg.lateWaitMs === undefined ? {} : { lateWaitMs: cfg.lateWaitMs }),
    log: { warn(message: string) {
      if (message.includes("stays queued until the next transaction")) say("QUEUED-WARN");
      else if (message.includes("it is queued again")) say("REQUEUED");
      else if (message.includes("its record stays open")) say("EXHAUSTED");
    } },
    testHooks: { afterLateSplice: async () => {
      if (!spliceHook) return;
      spliceHook = false;
      say("SPLICED");
      await waitFifo(cfg.holdGo!);
    } },
  });
  const hooked = { authority: rt.hooks, authorityRequired: true } as const;
  let closed = 0;
  const gitLog = forwardingLogger((_level, message) => {
    if (message.startsWith("runner-uid delete exited and left the unsettled record")) say(`CLOSED:${++closed}`);
  });
  const rmTimeout = cfg.rmTimeoutMs ?? 100;
  const rigs = [cfg.barrier, cfg.barrier2].filter((b): b is Barrier => b !== null && b !== undefined);
  const git = cfg.scenario === "pin-cleanup" ? new GitCache(cfg.dataDir, gitLog, undefined, hooked) : new GitCache(cfg.dataDir, gitLog, undefined, {
    ...hooked,
    retentionDelete: {
      split: true,
      spawn: rmSpawner(rigs),
      // The kill is a no-op: the held rm stays alive past the timeout, so its late close is the only join.
      kill: () => true,
      timeoutMs: rmTimeout,
      closeWaitMs: rmTimeout,
    },
  });
  const reseed = { beforeFree: async () => {}, isLive: () => false };
  const deleteClone = async (branch: string, key: string, runId: string): Promise<void> => {
    try {
      await rt.withTransaction(() => git.runnerCloneForBranch(cfg.bare, branch, key, reseed, runId));
      say("UNEXPECTED-CLEAN");
    } catch (error) {
      say(`UNSETTLED:${error instanceof Error ? error.constructor.name : "?"}`);
    }
  };

  const abort = new AbortController();
  let producer: Promise<unknown> | undefined;
  let signalled = false;
  const shutdownLog: ShutdownLog = forwardingLogger((level, message, fields) => {
    if (message.startsWith("worker_shutdown")) say(`LOG:${JSON.stringify({ level, message, fields })}`);
  });
  const onSigterm = (): void => {
    if (signalled) { say("SIGTERM-REPEAT"); return; }
    signalled = true;
    say("SIGTERM-HANDLER");
    const deadlineAt = Date.now() + (cfg.shutdownBudgetMs ?? 1_500);
    const observer = armShutdownResultDeadline({ deadlineAt, log: shutdownLog, snapshot: () => rt.snapshot() });
    abort.abort();
    void (async () => {
      await producer?.catch(() => undefined);
      const gate = await finishWorkerShutdown({
        authority: rt, git, deadlineAt, log: shutdownLog,
        testHooks: { onShutdownPhase: async (phase) => {
          say(`PHASE:${phase}`);
          if (phase === "sealed") {
            // A new outermost admission after the seal must be refused.
            await rt.withTransaction(async () => undefined).then(
              () => say("ADMISSION:allowed"),
              (error: unknown) => say(`ADMISSION:${error instanceof Error ? error.constructor.name : "?"}`));
          }
        } },
      });
      observer.clear();
      say(`RESULT:${JSON.stringify(gate)}`);
      process.exitCode = gate.exitCode;
      process.stdin.pause();
      process.stdin.destroy();
    })();
  };
  process.on("SIGTERM", onSigterm);

  say("W");
  if (cfg.scenario === "late-queue") {
    const mode = cfg.lateMode ?? "plain";
    await deleteClone(cfg.branch!, cfg.key!, cfg.runId!);
    if (mode === "a1") await deleteClone(cfg.branch2!, cfg.key2!, cfg.runId2!);
    if (mode === "a1") spliceHook = true;
    if (mode === "a3") writeGate.faultRename = true;
    if (mode === "a2") {
      // A holder of A that is NOT runtime.withTransaction: the late settlement can only queue behind it.
      let holding!: () => void;
      const isHolding = new Promise<void>((resolve) => { holding = resolve; });
      producer = withAuthorityTransaction(lifetime, { timeoutMs: 20_000 }, async () => { holding(); await waitFifo(cfg.holdGo!); });
      await isHolding;
      say("HOLDING-A");
    }
    say("READY");
  } else if (cfg.scenario === "late-close-inline") {
    const rig = cfg.barrier!;
    let reached!: () => void;
    const reachedWrite = new Promise<void>((resolve) => { reached = resolve; });
    writeGate.reached = reached;
    producer = rt.withTransaction(async () => {
      try {
        await git.runnerCloneForBranch(cfg.bare, cfg.branch!, cfg.key!, reseed, cfg.runId!);
        say("UNEXPECTED-CLEAN");
      } catch (error) {
        say(`UNSETTLED:${error instanceof Error ? error.constructor.name : "?"}`);
      }
      await waitFifo(rig.armed);
      writeGate.armed = true;
      say("RELEASING-RM");
      await openFifo(rig.go);
      await waitFifo(rig.done);
      // The late close fired inside this still-open transaction and its settlement write is paused at the barrier.
      let giveUp: NodeJS.Timeout | undefined;
      await Promise.race([reachedWrite, new Promise<void>((resolve) => {
        giveUp = setTimeout(() => { say("INLINE-NOT-REACHED"); resolve(); }, 30_000);
      })]);
      clearTimeout(giveUp);
      // Reported from inside the callback: with the transaction's owner joining the paused inline apply, the producer
      // promise itself only settles once that write finishes.
      say("CALLBACK-RETURNED");
    });
    await producer;
  } else {
    // pin-cleanup: the real `git update-ref --stdin` child of cleanupArchivedRecoveryPin sits behind a FIFO.
    const rig = cfg.barrier!;
    const pin = cfg.pin!;
    const wrapper = path.join(cfg.wrapDir!, "git");
    fs.writeFileSync(wrapper, [
      "#!/bin/sh",
      "hit=0; stdin=0",
      'for a in "$@"; do [ "$a" = update-ref ] && hit=1; [ "$a" = --stdin ] && stdin=1; done',
      'if [ "$hit$stdin" = 11 ]; then',
      `  echo $$ > '${rig.pid}'; echo armed > '${rig.armed}'; read _ < '${rig.go}'`,
      "fi",
      'exec /usr/bin/git "$@"', "",
    ].join("\n"), { mode: 0o755 });
    process.env.PATH = `${cfg.wrapDir}:${process.env.PATH}`;
    setTimeout(() => say("DEADLINE-PASSED"), pin.deadlineMs + 100);
    const deadline = Date.now() + pin.deadlineMs;
    producer = rt.withTransaction(() => git.cleanupArchivedRecoveryPin({
      runId: pin.runId, generation: pin.generation, sourceSha: pin.sourceSha,
      recoveryPinBareDir: path.basename(cfg.bare), isEligible: () => true, deadline,
    }));
    say(`CLEANUP:${JSON.stringify(await producer)}`);
  }
  process.stdin.resume();
}

async function main(): Promise<void> {
  const cfg = JSON.parse(process.argv[2]!) as ChildConfig;
  if (cfg.scenario === "late-queue" || cfg.scenario === "late-close-inline" || cfg.scenario === "pin-cleanup") {
    await runShutdownScenario(cfg);
    return;
  }
  const lifetime = await acquireWorkerLifetime(cfg.dataDir, testLockOptions());
  const rt = new AuthorityRuntime({ lifetime, workerToken: cfg.token, timeoutMs: 20_000 });
  const hooked = {
    authority: rt.hooks,
    authorityRequired: true,
  } as const;
  say("W");
  if (cfg.scenario === "delete") {
    const git = new GitCache(cfg.dataDir, quiet, undefined, {
      ...hooked,
      retentionDelete: {
        split: true,
        spawn: (_command, args) => {
          const target = args.at(-1)!;
          if (cfg.barrier) {
            const b = cfg.barrier;
            return spawn("/bin/sh", ["-c",
              'echo $$ > "$4"; echo armed > "$1"; read _ < "$2"; /bin/rm -rf -- "$5"; echo $? > "$3"',
              "sh", b.armed, b.go, b.done, b.pid, target], { cwd: "/", detached: true, stdio: ["ignore", "ignore", "pipe"] });
          }
          return spawn("/bin/rm", ["-rf", "--", target], { cwd: "/", detached: true, stdio: ["ignore", "ignore", "pipe"] });
        },
      },
    });
    const clone = await rt.withTransaction(() => git.runnerCloneForBranch(cfg.bare, cfg.branch!, cfg.key!,
      { beforeFree: async () => {}, isLive: () => false }, cfg.runId));
    // Production ends the flight's source use once its tails are joined; only then may a successor admit the path.
    const ended = await rt.endSourceUse([clone.path]);
    say(`DONE:${JSON.stringify(ended)}`);
  } else {
    const git = new GitCache(cfg.dataDir, quiet, undefined, hooked);
    let seen = 0;
    const internals = git as unknown as {
      execScoped(command: string, args: string[], options: { env?: NodeJS.ProcessEnv; cwd?: string }): Promise<{ stdout: string; stderr: string }>;
    };
    const original = internals.execScoped.bind(git);
    internals.execScoped = (command, args, options) => {
      const isDelete = command === "git" && args.includes("update-ref") && args.includes("-d");
      if (!isDelete || !cfg.barrier || ++seen !== (cfg.pauseAtDelete ?? 1)) return original(command, args, options);
      const b = cfg.barrier;
      return new Promise((resolve, reject) => {
        const child = spawn("/bin/sh", ["-c",
          'd=$3; echo $$ > "$4"; echo armed > "$1"; read _ < "$2"; shift 4; "$@"; echo $? > "$d"',
          "sh", b.armed, b.go, b.done, b.pid, command, ...args],
        { cwd: options.cwd ?? "/", env: options.env ?? process.env, detached: true, stdio: ["ignore", "pipe", "pipe"] });
        let stdout = "", stderr = "";
        child.stdout!.on("data", (c: Buffer) => { stdout += c.toString("utf8"); });
        child.stderr!.on("data", (c: Buffer) => { stderr += c.toString("utf8"); });
        child.once("error", reject);
        child.once("close", (code) => code === 0 ? resolve({ stdout, stderr })
          : reject(Object.assign(new Error(`git exited ${code}: ${stderr}`), { code, stdout, stderr })));
      });
    };
    const result = await rt.withTransaction(() => git.cleanupRecoveryGeneration(cfg.bare, cfg.context!, cfg.frozen!,
      cfg.coveragePins!, cfg.recoverySources!, cfg.coveringSha!));
    say(`DONE:${result}`);
  }
  process.stdin.resume();
}

main().catch((error: unknown) => {
  say(`FAILED:${error instanceof Error ? error.message : String(error)}`);
  process.exit(3);
});
