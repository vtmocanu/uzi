// Optional Go build concurrency defaults for scrubbed command envs (issue #3382).
//
// WORKER_GO_BUILD_PARALLELISM becomes GOFLAGS=-p=N and WORKER_GO_MAXPROCS becomes
// GOMAXPROCS=N in the env of the commands the worker launches (agent Bash, Codex
// commands, checks, the PR-description helper). WORKER_GO_MEMLIMIT (`<n>MiB|GiB`)
// becomes GOMEMLIMIT via applyGoMemLimitDefault, which yields to a run variable named
// GOMEMLIMIT. All are parsed ONCE at startup;
// unset or blank means the key is absent. Nothing is passed through from the host
// GOFLAGS or any other variable: the scrubbed env builders stay allowlists.

export interface GoConcurrency {
  buildParallelism?: number;
  maxProcs?: number;
  /** Canonical GOMEMLIMIT value, `<n>MiB` or `<n>GiB`. */
  memLimit?: string;
}

const MEM_LIMIT_ERROR = `WORKER_GO_MEMLIMIT must be a positive integer <= 2147483647 followed by MiB or GiB`;

function optionalMemLimit(env: NodeJS.ProcessEnv): string | undefined {
  const raw = (env.WORKER_GO_MEMLIMIT ?? "").replace(/^[ \t\r\n]+|[ \t\r\n]+$/g, "");
  if (raw === "") return undefined;
  const m = /^([0-9]+)(MiB|GiB)$/.exec(raw);
  if (!m) throw new Error(MEM_LIMIT_ERROR);
  const n = parseBoundedPositiveInt((m[1] ?? "").replace(/^0+(?=[0-9])/, ""), GO_CONCURRENCY_CEILING);
  if (n === undefined) throw new Error(MEM_LIMIT_ERROR);
  return `${String(n)}${m[2]}`;
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

/** Parse the three optional worker settings; throws an Error naming the bad variable. */
export function parseGoConcurrency(env: NodeJS.ProcessEnv): GoConcurrency {
  const out: GoConcurrency = {};
  const parallelism = optionalGoSetting(env, "WORKER_GO_BUILD_PARALLELISM");
  if (parallelism !== undefined) out.buildParallelism = parallelism;
  const maxProcs = optionalGoSetting(env, "WORKER_GO_MAXPROCS");
  if (maxProcs !== undefined) out.maxProcs = maxProcs;
  const memLimit = optionalMemLimit(env);
  if (memLimit !== undefined) out.memLimit = memLimit;
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

/**
 * Default GOMEMLIMIT into `env` from the settings (default: the configured ones). It
 * yields to a run variable the fold injected (`injected` names GOMEMLIMIT); a GOMEMLIMIT
 * that arrived any other way (toolEnv, host) is overwritten. No-op when unset.
 */
export function applyGoMemLimitDefault(
  env: Record<string, string | undefined>,
  injected?: readonly { name: string }[],
  settings: GoConcurrency = configured,
): void {
  if (settings.memLimit === undefined) return;
  if (injected?.some((v) => v.name === "GOMEMLIMIT")) return;
  env.GOMEMLIMIT = settings.memLimit;
}
