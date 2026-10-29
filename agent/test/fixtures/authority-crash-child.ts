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
import { acquireWorkerLifetime } from "../../src/authority-lock.js";
import { AuthorityRuntime } from "../../src/authority-runtime.js";
import { GitCache, type PositiveOwedCandidateContext } from "../../src/git.js";
import type { Logger } from "../../src/log.js";
import { testLockOptions } from "../authority-test-support.js";

export interface Barrier { armed: string; go: string; done: string; pid: string }
export interface ChildConfig {
  scenario: "delete" | "cleanup";
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
}

const quiet: Logger = {
  debug() {}, info() {}, warn() {}, error() {}, addSecret() {}, removeSecret() {},
  child() { return quiet; },
};

const say = (line: string): void => { process.stdout.write(`${line}\n`); };

async function main(): Promise<void> {
  const cfg = JSON.parse(process.argv[2]!) as ChildConfig;
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
