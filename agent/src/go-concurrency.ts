// Optional Go build concurrency defaults for scrubbed command envs (issue #3382).
//
// WORKER_GO_BUILD_PARALLELISM becomes GOFLAGS=-p=N and WORKER_GO_MAXPROCS becomes
// GOMAXPROCS=N in the env of the commands the worker launches (agent Bash, Codex
// commands, checks, the PR-description helper). Both are parsed ONCE at startup;
// unset or blank means the key is absent. Nothing is passed through from the host
// GOFLAGS or any other variable: the scrubbed env builders stay allowlists.

export interface GoConcurrency {
  buildParallelism?: number;
  maxProcs?: number;
}

/** The int32 ceiling shared with the controller/chart validation. */
const GO_CONCURRENCY_CEILING = 2147483647;

/** Parse a trimmed, digits-only positive integer no larger than `ceiling`; undefined
 *  when it is anything else (sign, fraction, exponent, trailing text, unsafe size).
 *  Shared by the memory guard in config.ts and the Go concurrency settings. */
export function parseBoundedPositiveInt(raw: string, ceiling: number): number | undefined {
  const value = Number(raw);
  if (!/^[0-9]+$/.test(raw) || !Number.isSafeInteger(value) || value <= 0 || value > ceiling) return undefined;
  return value;
}

function optionalGoSetting(env: NodeJS.ProcessEnv, name: string): number | undefined {
  const raw = env[name]?.trim() ?? "";
  if (raw === "") return undefined;
  const value = parseBoundedPositiveInt(raw, GO_CONCURRENCY_CEILING);
  if (value === undefined) {
    throw new Error(`${name} must be a positive integer <= ${GO_CONCURRENCY_CEILING}`);
  }
  return value;
}

/** Parse the two optional worker settings; throws an Error naming the bad variable. */
export function parseGoConcurrency(env: NodeJS.ProcessEnv): GoConcurrency {
  const out: GoConcurrency = {};
  const parallelism = optionalGoSetting(env, "WORKER_GO_BUILD_PARALLELISM");
  if (parallelism !== undefined) out.buildParallelism = parallelism;
  const maxProcs = optionalGoSetting(env, "WORKER_GO_MAXPROCS");
  if (maxProcs !== undefined) out.maxProcs = maxProcs;
  return out;
}

let configured: Readonly<GoConcurrency> = Object.freeze({});

/** Store the process-level settings (a frozen copy). `{}` resets. */
export function configureGoConcurrency(settings: GoConcurrency): void {
  configured = Object.freeze({ ...settings });
}

/**
 * The env keys for the given settings (default: the configured ones); only set keys
 * are present. `trustedGoflags` is a worker-authored GOFLAGS base (never host input)
 * that the -p flag is appended to.
 */
export function goConcurrencyEnv(
  settings: GoConcurrency = configured,
  trustedGoflags?: string,
): Record<string, string> {
  const out: Record<string, string> = {};
  const p = settings.buildParallelism === undefined ? undefined : `-p=${String(settings.buildParallelism)}`;
  if (p !== undefined) out.GOFLAGS = trustedGoflags ? `${trustedGoflags} ${p}` : p;
  else if (trustedGoflags) out.GOFLAGS = trustedGoflags;
  if (settings.maxProcs !== undefined) out.GOMAXPROCS = String(settings.maxProcs);
  return out;
}
