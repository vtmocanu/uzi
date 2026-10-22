import { spawn } from "node:child_process";

const maxOutputBytes = 4 * 1024 * 1024;
const bounded = (value: unknown) => {
  if (value == null) return null;
  const text = Buffer.from(String(value)).subarray(0, 4096).toString();
  // A replacement character at a split UTF-8 boundary can exceed the byte budget.
  return Buffer.byteLength(text) > 4096 ? text.slice(0, -1) : text;
};

export function runnerFailure(error: unknown): Record<string, unknown> {
  const fields = error as Record<string, unknown> | null;
  return {
    error: bounded(error), code: bounded(fields?.code), syscall: bounded(fields?.syscall),
    killed: fields?.killed === true, signal: bounded(fields?.signal),
    stdout: bounded(fields?.stdout), stderr: bounded(fields?.stderr),
  };
}

export async function runRunner(
  runnerCommand: (command: string, args: string[]) => { command: string; args: string[] },
  command: string, args: string[],
  options: { cwd: string; env: NodeJS.ProcessEnv; timeout: number;
    killRunnerGroupOnly: (pgid: number) => boolean;
    record: (kind: string, data: Record<string, unknown>) => void },
) {
  const { cwd, env, timeout, record, killRunnerGroupOnly } = options;
  const context = { command: bounded(command), cwd: bounded(cwd), timeoutMs: timeout };
  let stage = "wrap";
  try {
    record("runner-launch", context);
    const wrapped = runnerCommand(command, args);
    stage = "exec";
    const result = await new Promise<{ stdout: string; stderr: string }>((resolve, reject) => {
      const child = spawn(wrapped.command, wrapped.args, {
        cwd, env, detached: true, stdio: ["ignore", "pipe", "pipe"],
      });
      const stdout: Buffer[] = [], stderr: Buffer[] = [];
      const sizes = { stdout: 0, stderr: 0 };
      let failure: Error | undefined;
      let cleanupAttempted = false;
      let settled = false;
      const cleanup = (reason: string) => {
        if (cleanupAttempted || child.pid === undefined) return;
        cleanupAttempted = true;
        let dispatched = false;
        let groupAbsent = false;
        let cleanupError: unknown;
        try {
          dispatched = killRunnerGroupOnly(child.pid);
        } catch (error) {
          cleanupError = error;
          failure ??= error instanceof Error ? error : new Error(String(error));
        }
        if (!dispatched) {
          // Probe only our detached child's known PGID. EPERM proves existence;
          // only ESRCH proves absence. No single-PID fallback is permitted.
          try { process.kill(-child.pid, 0); }
          catch (error) {
            groupAbsent = (error as NodeJS.ErrnoException).code === "ESRCH";
          }
          if (!groupAbsent) {
            failure ??= Object.assign(new Error("Runner group cleanup failed"), {
              code: "ERR_RUNNER_GROUP_CLEANUP", killed: false,
            });
          }
        }
        record("runner-cleanup", { ...context, reason, pgid: child.pid, dispatched,
          groupAbsent, error: bounded(cleanupError) });
      };
      const settle = (code: number | null, signal: NodeJS.Signals | null) => {
        if (settled) return;
        settled = true;
        clearTimeout(timer);
        const output = {
          stdout: Buffer.concat(stdout, sizes.stdout).toString(),
          stderr: Buffer.concat(stderr, sizes.stderr).toString(),
        };
        if (!failure && (code !== 0 || signal !== null)) {
          failure = Object.assign(new Error("Command failed: " + wrapped.command), { code, signal, killed: false });
        }
        if (failure) {
          Object.assign(failure, output);
          reject(failure);
        } else resolve(output);
      };
      const timer = setTimeout(() => {
        failure ??= Object.assign(new Error("Runner deadline exceeded"), { killed: false });
        cleanup("deadline");
        // The completion bound includes inherited pipes and failed signals.
        // Host/container cleanup retains ownership of any surviving group.
        child.stdout.destroy();
        child.stderr.destroy();
        child.unref();
        settle(null, null);
      }, timeout);
      // Each stream keeps at most maxOutputBytes, including the final partial chunk.
      // First overflow closes both pipes and settles without waiting for this group.
      // Cleanup is attempted once; host/container cleanup owns any surviving group.
      const capture = (name: "stdout" | "stderr", chunks: Buffer[], chunk: Buffer) => {
        if (settled) return;
        const remaining = maxOutputBytes - sizes[name];
        if (remaining > 0) {
          const kept = chunk.subarray(0, remaining);
          chunks.push(Buffer.from(kept));
          sizes[name] += kept.length;
        }
        if (chunk.length > remaining) {
          failure ??= Object.assign(new RangeError(name + " maxBuffer length exceeded"),
            { code: "ERR_CHILD_PROCESS_STDIO_MAXBUFFER", killed: false });
          child.stdout.destroy();
          child.stderr.destroy();
          cleanup("overflow");
          child.unref();
          settle(null, null);
        }
      };
      child.stdout.on("data", (chunk: Buffer) => capture("stdout", stdout, chunk));
      child.stderr.on("data", (chunk: Buffer) => capture("stderr", stderr, chunk));
      child.on("error", (error) => {
        failure ??= error;
        cleanup("error");
      });
      // exit precedes close when a descendant still holds the output pipes.
      child.on("exit", () => cleanup("exit"));
      child.on("close", (code, signal) => {
        cleanup("close");
        settle(code, signal);
      });
    });
    record("runner-complete", context);
    return result;
  } catch (error) {
    record("runner-error", { ...context, stage, ...runnerFailure(error) });
    throw error;
  }
}
