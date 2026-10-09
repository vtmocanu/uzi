// Evaluator-only, same-identity app-server owner. No worker binding or UID split.
import { spawn, type ChildProcess } from "node:child_process";
import fs from "node:fs/promises";
import path from "node:path";
import { CODEX_BIN, PROVIDER_CHILD_ARGV } from "./launcher.js";
import { assertNoUnexpectedSystemConfig, buildCodexProductionConfigToml } from "./config.js";
import { createCodexTransport, type CodexTransport } from "./transport.js";
import type { LaunchAdviceRootSeam } from "./codex-advice-harness.js";

/** Credential-free lifecycle doubles only; never an alternate provider/auth adapter. */
export interface PrDescriptionEvalLaunchTestSeams {
  spawn?: (bin: string, argv: string[], opts: {
    cwd: string; env: NodeJS.ProcessEnv; stdio: ["pipe", "pipe", "pipe"];
  }) => ChildProcess;
  transport?: typeof createCodexTransport;
  /** Readiness can resolve late, but cannot return resources: ownership precedes it. */
  startup?: (child: ChildProcess, signal: AbortSignal) => Promise<void>;
  terminateGraceMs?: number;
}

export function makePrDescriptionEvalLaunch(
  checkoutPath: string,
  signal?: AbortSignal,
  testSeams: PrDescriptionEvalLaunchTestSeams = {},
): { launchRoot: LaunchAdviceRootSeam; close: () => Promise<void>; signal: AbortSignal } {
  const abort = new AbortController();
  let admitted = false;
  let closed = false;
  let root: string | undefined;
  let child: ChildProcess | undefined;
  let exited = false;
  let exit: Promise<void> = Promise.resolve();
  let transport: CodexTransport | undefined;
  let pending: Promise<unknown> | undefined;
  let closing: Promise<void> | undefined;
  let launchSignal: AbortSignal | undefined;
  const onAbort = (): void => {
    abort.abort();
    void close().catch(() => {}); // run/explicit close observes the cleanup failure.
  };
  if (signal?.aborted) abort.abort();
  else signal?.addEventListener("abort", onAbort, { once: true });

  function close(): Promise<void> {
    if (closing) return closing;
    closed = true;
    closing = (async () => {
      // Provisioning must settle before removal; readiness is raced against abort.
      await pending?.catch(() => {});
      let failure: unknown;
      try {
        await transport?.close();
      } catch {
        failure = new Error("evaluator transport cleanup failed");
      } finally {
        if (child && !exited) {
          child.kill("SIGTERM");
          // One TERM attempt, one bounded grace, then one KILL; removal awaits exit.
          let timer: NodeJS.Timeout | undefined;
          await Promise.race([exit, new Promise<void>((resolve) => {
            timer = setTimeout(resolve, testSeams.terminateGraceMs ?? 1000);
          })]);
          if (timer) clearTimeout(timer);
          if (!exited) child.kill("SIGKILL");
          await exit;
        }
        if (root) await fs.rm(root, { recursive: true, force: true });
        signal?.removeEventListener("abort", onAbort);
        launchSignal?.removeEventListener("abort", onAbort);
      }
      if (failure) throw failure;
    })();
    // Publish the close promise before abort listeners can re-enter close.
    abort.abort();
    return closing;
  }

  const launchRoot: LaunchAdviceRootSeam = (spec) => {
    if (closed || admitted || abort.signal.aborted) {
      return Promise.reject(new Error("evaluator is closed or already used"));
    }
    admitted = true;
    launchSignal = spec.signal;
    if (spec.signal?.aborted) abort.abort();
    else spec.signal?.addEventListener("abort", onAbort, { once: true });
    pending = (async () => {
      abort.signal.throwIfAborted();
      assertNoUnexpectedSystemConfig();
      const checkout = await fs.realpath(checkoutPath);
      const parent = path.join(checkout, ".uzi");
      const scratch = path.join(parent, "scratch");
      // Reject symlinked parents before writing any evaluator data.
      await fs.mkdir(parent, { recursive: true, mode: 0o700 });
      if (!(await fs.lstat(parent)).isDirectory()) throw new Error("evaluator scratch parent is not a directory");
      await fs.mkdir(scratch, { recursive: true, mode: 0o700 });
      if (!(await fs.lstat(scratch)).isDirectory()) throw new Error("evaluator scratch is not a directory");
      abort.signal.throwIfAborted();
      root = await fs.mkdtemp(path.join(scratch, "pr-description-eval-"));
      await fs.chmod(root, 0o700);
      const dirs = ["cwd", "home", "codex-home", "config", "cache", "data", "state", "runtime", "tmp"];
      // Nine fixed directories, no retries; a failure stops setup and closes this owner.
      for (const dir of dirs) {
        abort.signal.throwIfAborted();
        await fs.mkdir(path.join(root, dir), { mode: 0o700 });
        await fs.chmod(path.join(root, dir), 0o700);
      }
      const cwd = path.join(root, "cwd");
      const env: NodeJS.ProcessEnv = {
        PATH: "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
        LANG: "C.UTF-8",
        HOME: path.join(root, "home"),
        CODEX_HOME: path.join(root, "codex-home"),
        XDG_CONFIG_HOME: path.join(root, "config"),
        XDG_CACHE_HOME: path.join(root, "cache"),
        XDG_DATA_HOME: path.join(root, "data"),
        XDG_STATE_HOME: path.join(root, "state"),
        XDG_RUNTIME_DIR: path.join(root, "runtime"),
        TMPDIR: path.join(root, "tmp"),
        TMP: path.join(root, "tmp"),
        TEMP: path.join(root, "tmp"),
      };
      await fs.writeFile(path.join(env.CODEX_HOME!, "config.toml"), buildCodexProductionConfigToml({
        model: spec.model, projectPath: cwd, authMode: "api_key", codeModeHost: false,
      }), { mode: 0o600 });
      abort.signal.throwIfAborted();
      child = (testSeams.spawn ?? spawn)(CODEX_BIN, [...PROVIDER_CHILD_ARGV], {
        cwd, env, stdio: ["pipe", "pipe", "pipe"],
      });
      // Own the exit and pipes synchronously, before any startup await.
      exit = new Promise<void>((resolve) => {
        child!.once("close", () => { exited = true; resolve(); });
      });
      const spawned = new Promise<void>((resolve, reject) => {
        child!.once("spawn", resolve);
        child!.once("error", () => reject(new Error("evaluator app-server startup failed")));
      });
      // Always consume startup failure, including injected readiness.
      void spawned.catch(() => {});
      child.stderr?.resume();
      if (!child.stdout || !child.stdin) throw new Error("evaluator app-server has no transport");
      transport = (testSeams.transport ?? createCodexTransport)({
        inbound: child.stdout, outbound: child.stdin,
      });
      let cancel: (() => void) | undefined;
      const cancelled = new Promise<never>((_, reject) => {
        cancel = () => reject(new Error("evaluator startup aborted"));
        if (abort.signal.aborted) cancel();
        else abort.signal.addEventListener("abort", cancel, { once: true });
      });
      try {
        await Promise.race([
          testSeams.startup ? testSeams.startup(child, abort.signal) : spawned,
          cancelled,
        ]);
        abort.signal.throwIfAborted();
        return { transport, cwd, dispose: close };
      } finally {
        if (cancel) abort.signal.removeEventListener("abort", cancel);
      }
    })();
    void pending.catch(() => { void close().catch(() => {}); });
    return pending as ReturnType<LaunchAdviceRootSeam>;
  };
  return { launchRoot, close, signal: abort.signal };
}
