// Issue #2852: internal capability plumbing. No generic command API is exposed by
// the context. GitCache is the only producer of authorized capture requests.
import { isAbsolute, resolve } from "node:path";
import type { BoundaryProcessHandle, BoundaryProcessRequest, CredentialFreeCaptureContext } from "./harness.js";
import { TrustedExecutionRefusal } from "./trusted-execution-refusal.js";
import { carriesForgeCredential } from "./residue-quarantine.js";

type Spawner = (request: BoundaryProcessRequest) => Promise<BoundaryProcessHandle>;
interface Acquisition {
  readonly spawn: Spawner;
  readonly live: () => boolean;
}
const acquisitions = new WeakMap<CredentialFreeCaptureContext, Acquisition>();
const requests = new WeakMap<BoundaryProcessRequest, { context: CredentialFreeCaptureContext; validate: () => void }>();
interface Launch {
  readonly command: string;
  readonly args: readonly string[];
  readonly cwd: string;
  readonly env: NodeJS.ProcessEnv;
  readonly identity: string;
}
const launches = new WeakMap<object, () => void>();

function refuse(): never {
  throw new TrustedExecutionRefusal("credential-free capture launch refused");
}

/** Safety alone creates an acquisition, after the registry observed_empty check. */
export function createCaptureContext(
  signal: AbortSignal, deadlineAt: number, spawn: Spawner, live: () => boolean,
): CredentialFreeCaptureContext {
  const context = Object.freeze({ signal, deadlineAt }) as CredentialFreeCaptureContext;
  acquisitions.set(context, { spawn, live });
  return context;
}

function acquisition(context: CredentialFreeCaptureContext): Acquisition {
  const held = acquisitions.get(context);
  if (!held || !held.live() || context.signal.aborted || Date.now() >= context.deadlineAt) refuse();
  return held;
}

/** Bind privately inside GitCache; callers receive no spawner on the context. */
export function captureGitSpawner(
  context: CredentialFreeCaptureContext,
  validate: (request: BoundaryProcessRequest) => void,
): Spawner {
  acquisition(context);
  return (raw) => {
    acquisition(context);
    // Copy before the first await: no retained caller-owned array or env survives.
    const request = Object.freeze({ ...raw, argv: Object.freeze([...raw.argv]), env: Object.freeze({ ...raw.env }) });
    const check = () => { acquisition(context); validate(request); };
    check();
    requests.set(request, { context, validate: check });
    return acquisition(context).spawn(request);
  };
}

export function assertCaptureRequest(context: CredentialFreeCaptureContext, request: BoundaryProcessRequest): void {
  const held = requests.get(request);
  if (!held || held.context !== context) refuse();
  held.validate();
}

export function isCaptureRequest(request: BoundaryProcessRequest): boolean {
  return requests.has(request);
}

/** The executor binds its trusted identity/sandbox transformation before launch. */
export function bindCaptureLaunch(request: BoundaryProcessRequest, spec: Launch): void {
  const held = requests.get(request);
  if (!held) return;
  held.validate();
  const checkTransformation = () => {
    if (spec.identity !== request.identity || spec.cwd !== request.cwd) refuse();
    let argv: readonly string[];
    if (request.identity === "command") {
      const tmp = spec.env.HOME;
      if (!tmp || !/^\/tmp\/uzi-codex-command-[0-9a-f-]{36}$/.test(tmp) ||
          spec.env.TMPDIR !== tmp || spec.command !== "/usr/local/bin/uzi-codex-command-sandbox" ||
          spec.args[0] !== "--root" || spec.args[1] !== request.cwd ||
          spec.args[2] !== "--tmp" || spec.args[3] !== tmp ||
          spec.args[4] !== "--cwd" || spec.args[5] !== request.cwd ||
          spec.args[6] !== "--mode" || spec.args[8] !== "--") refuse();
      argv = spec.args.slice(9);
    } else argv = [spec.command, ...spec.args];
    if (argv.length !== request.argv.length || argv.some((arg, i) => arg !== request.argv[i])) refuse();
    const keys = new Set([...Object.keys(spec.env), ...Object.keys(request.env)]);
    for (const key of keys) {
      if (request.identity === "command" && (key === "HOME" || key === "TMPDIR")) continue;
      if (spec.env[key] !== request.env[key] || Object.hasOwn(spec.env, key) !== Object.hasOwn(request.env, key)) refuse();
    }
  };
  checkTransformation();
  const command = spec.command;
  const args = Object.freeze([...spec.args]);
  const cwd = spec.cwd;
  const identity = spec.identity;
  const env = Object.freeze({ ...spec.env });
  Object.freeze(spec.args);
  Object.freeze(spec.env);
  Object.freeze(spec);
  launches.set(spec, () => {
    held.validate();
    checkTransformation();
    if (spec.command !== command || spec.cwd !== cwd || spec.identity !== identity ||
        spec.args.length !== args.length || spec.args.some((arg, i) => arg !== args[i]) ||
        Object.keys(spec.env).length !== Object.keys(env).length ||
        Object.entries(spec.env).some(([key, value]) => env[key] !== value)) refuse();
  });
}

/** Synchronous final check at the real supervisor spawn, after all wrapper awaits. */
export function assertCaptureLaunch(spec: Launch): void {
  launches.get(spec)?.();
}

function forbiddenConfig(key: string): boolean {
  return /^(credential\.|core\.(?:.*\.)?askpass$|http\..*extraheader$|include\.|includeif\.)/i.test(key);
}
function credentialText(value: string): boolean {
  return /authorization:/i.test(value) || /[a-z][a-z0-9+.-]*:\/\/[^/\s]*@/i.test(value);
}

/** Complete canonical env comparison includes indexed config ORDER and every key.
 * The supplied variants are built by GitCache, never by a runner. */
export function validateCaptureGitRequest(
  request: BoundaryProcessRequest,
  environments: readonly NodeJS.ProcessEnv[],
  gitBin: string,
  helperBin: string,
  cancelContentHelper: string,
): void {
  if (request.recoverableTimeout || !isAbsolute(request.cwd) || resolve(request.cwd) !== request.cwd) refuse();
  const env = request.env;
  const trusted = environments.some(candidate =>
    Object.keys(env).length === Object.keys(candidate).length &&
    Object.entries(env).every(([key, value]) => candidate[key] === value));
  if (!trusted || carriesForgeCredential(env)) refuse();
  for (const [key, value] of Object.entries(env)) {
    if (/^(?:GIT_CONFIG_PARAMETERS|GIT_ASKPASS|SSH_ASKPASS|GIT_CREDENTIAL_.*|GCM_.*)$/i.test(key) ||
        credentialText(key) || (value !== undefined && credentialText(value))) refuse();
  }
  const count = Number(env.GIT_CONFIG_COUNT);
  if (!Number.isSafeInteger(count) || count < 2 || String(count) !== env.GIT_CONFIG_COUNT) refuse();
  let helpers = 0;
  let askpasses = 0;
  for (let i = 0; i < count; i++) {
    const key = env[`GIT_CONFIG_KEY_${i}`];
    const value = env[`GIT_CONFIG_VALUE_${i}`];
    if (key === "credential.helper" && value === "") helpers++;
    else if (key === "core.askpass" && value === "") askpasses++;
    else if (!key || forbiddenConfig(key)) refuse();
  }
  if (helpers !== 1 || askpasses !== 1) refuse();
  if (request.argv[0] === helperBin) {
    // This is the embedded cancellation content reader only. Its internal git
    // environment is not represented as the outer canonical config.
    if (request.identity !== "command" || request.argv[1] !== "-e" ||
        request.argv[2] !== cancelContentHelper || request.argv[3] !== request.cwd ||
        (request.argv.length !== 4 && (request.argv.length !== 5 || !/^\d+$/.test(request.argv[4]!)))) refuse();
    return;
  }
  if (request.argv[0] !== gitBin) refuse();
  for (let i = 1; i < request.argv.length; i++) {
    const arg = request.argv[i]!;
    if (credentialText(arg) || /^--config-env/i.test(arg) || forbiddenConfig(arg.split("=")[0]!)) refuse();
    let config: string | undefined;
    if (arg === "-c") { config = request.argv[++i]; if (config === undefined) refuse(); }
    else if (arg.startsWith("-c") && arg.length > 2) config = arg.slice(2);
    if (config !== undefined) {
      const canonical = environments[0]!;
      const trustedPins = Array.from({ length: Number(canonical.GIT_CONFIG_COUNT) }, (_, n) =>
        `${canonical[`GIT_CONFIG_KEY_${n}`]}=${canonical[`GIT_CONFIG_VALUE_${n}`]}`);
      const readerPins = ["core.untrackedCache=false", "core.ignoreStat=false", "core.checkStat=default",
        "core.trustctime=true", "core.fileMode=true", "core.ignoreCase=false", "core.excludesFile=/dev/null",
        "commit.gpgsign=false"];
      if (forbiddenConfig(config.split("=")[0]!) || credentialText(config) ||
          (!trustedPins.includes(config) && !readerPins.includes(config))) refuse();
    }
  }
}
