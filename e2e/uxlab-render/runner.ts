import { execFile } from "node:child_process";
import { promisify } from "node:util";

const exec = promisify(execFile);
const killAfterMs = 5_000;
const fallbackGraceMs = 5_000;
const bounded = (value: unknown) => value == null ? null : String(value).slice(0, 4096);

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
    record: (kind: string, data: Record<string, unknown>) => void },
) {
  const { cwd, env, timeout, record } = options;
  const outerTimeoutMs = timeout + killAfterMs + fallbackGraceMs;
  const timeoutBinary = process.env.UXLAB_RENDER_RUNNER_TIMEOUT ?? "/opt/uzi-toolchain/bin/timeout";
  const context = { command: bounded(command), timeoutBinary: bounded(timeoutBinary), cwd: bounded(cwd), timeoutMs: timeout,
    killAfterMs, outerTimeoutMs };
  let stage = "wrap";
  try {
    record("runner-launch", context);
    // timeout runs after runnerCommand's uid drop and manages its own process group.
    // TERM at the deadline, KILL after the grace; execFile is only a later fallback.
    const wrapped = runnerCommand(timeoutBinary,
      ["--kill-after=5s", `${timeout / 1000}s`, command, ...args]);
    stage = "exec";
    const result = await exec(wrapped.command, wrapped.args,
      { cwd, env, timeout: outerTimeoutMs, maxBuffer: 4 * 1024 * 1024 });
    record("runner-complete", context);
    return result;
  } catch (error) {
    record("runner-error", { ...context, stage, ...runnerFailure(error) });
    throw error;
  }
}
