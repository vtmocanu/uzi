// Sparse credential env for the Claude Agent SDK subprocess (bottega's
// sparse-env pattern, tightened for uzi's trust boundary).
//
// The SDK `env` option REPLACES the subprocess environment entirely (it is not
// merged with process.env — see the SDK's Options.env docs), so the object we
// build here is exactly what the agent's Bash tool and every child it spawns
// will see. That is the load-bearing property behind the primary directive:
//
//   - The agent gets the worker-built replacement environment (the Anthropic
//     OAuth token, HOME, PATH and the other worker-set keys) plus, on a
//     lead-lane run, the permitted repo run variables at the lowest
//     precedence (ADR-4416).
//   - The worker's own secrets — the join token (UZI_WORKER_TOKEN) and the
//     GitLab bot PAT — are NEVER in this object, so they cannot leak into the
//     agent subprocess (the worker, not the agent, performs authenticated git).
//   - ANTHROPIC_API_KEY / ANTHROPIC_AUTH_TOKEN are pinned to `undefined`. They
//     outrank CLAUDE_CODE_OAUTH_TOKEN in the SDK's auth precedence; because env
//     is a full replacement they would already be absent, but setting them
//     explicitly to undefined documents intent and is defensive against a
//     future SDK version that changes to merge semantics.
//
// HOME is pinned onto the persistent data volume by the caller so the SDK's
// session transcripts under $HOME/.claude/projects survive a container restart
// (resume). PATH is inherited so the bundled Claude Code CLI can find `git`,
// `bash`, and coreutils.

import { BROWSER_DISCOVERY_ENV } from "./browser-env.js";
import { claudeConfigDir } from "./claude-config-dir.js";
import { goConcurrencyEnv } from "./go-concurrency.js";
import { runnerPath, runnerTmpdir, uidSplitActive } from "./runner-uid.js";
import { TrustedExecutionRefusal } from "./trusted-execution-refusal.js";
import type { ClaimRunEnvVar } from "./protocol.js";
import { foldRunEnv, runEnvInjectedSet, type RunEnvInjectedSet, type RunEnvInjection } from "./run-env-inject.js";
import { RUN_ATTEMPT_ENV, RUN_CLONE_ENV, RUN_CLONE_KEY_ENV, WORKER_SPAWN_ENV } from "./worker-spawn-mark.js";

/** Keys a provisioned tool env may NEVER set (PRD #18 M3): the OAuth credential,
 *  HOME, and the two ANTHROPIC_* keys pinned to undefined so nothing outranks the
 *  OAuth token in the SDK's auth precedence. */
export const PROTECTED_ENV_KEYS: ReadonlySet<string> = new Set([
  "CLAUDE_CODE_OAUTH_TOKEN",
  "HOME",
  // PRD #4416: where the CLI reads its global config from (claudeConfigDir(HOME)); spawnDetached
  // refuses any other value, and a provisioned or user variable must never move it.
  "CLAUDE_CONFIG_DIR",
  "ANTHROPIC_API_KEY",
  "ANTHROPIC_AUTH_TOKEN",
  // PRD #4320 module 6: the provider-mode routing and model keys. A provisioned tool env may never
  // repoint the CLI at another gateway, remap a model alias, or move the context window.
  "ANTHROPIC_BASE_URL",
  "ANTHROPIC_DEFAULT_OPUS_MODEL",
  "ANTHROPIC_DEFAULT_SONNET_MODEL",
  "ANTHROPIC_DEFAULT_HAIKU_MODEL",
  "ANTHROPIC_DEFAULT_FABLE_MODEL",
  "ANTHROPIC_SMALL_FAST_MODEL",
  "CLAUDE_CODE_SUBAGENT_MODEL",
  "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC",
  "CLAUDE_CODE_MAX_CONTEXT_TOKENS",
  "CLAUDE_CODE_AUTO_COMPACT_WINDOW",
  // The CLI's extra CA bundle (UZI_CLI_CA_FILE below). Worker-set; a provisioned var must
  // never point the CLI's TLS trust at a file of its choosing.
  "NODE_EXTRA_CA_CERTS",
  // Not a credential — the mandatory browser launch flag (see the base env below).
  // provision.ts's allowlist does not emit AGENT_BROWSER_ARGS today, so this is not a
  // live conflict; protecting it keeps the --no-sandbox guarantee durable against
  // allowlist drift, so a provisioned var can never overwrite or drop it.
  "AGENT_BROWSER_ARGS",
  // Issue #3907: the browser discovery paths (AGENT_BROWSER_EXECUTABLE_PATH, CHROME_PATH),
  // worker-set to the uzi-chromium wrapper after the fold below; a provisioned var can
  // never redirect discovery to an unwrapped Chromium. Defence in depth: the post-fold
  // assignment alone already wins; this keeps the guarantee if that ever becomes conditional.
  ...Object.keys(BROWSER_DISCOVERY_ENV),
  // Non-secret, worker-owned path to the immutable verified sqlc archive cache.
  "UZI_SQLC_DIR",
  // issue #1783 (R2/R4): the run-attempt marker and the worker spawn mark. The quiescence
  // reaper attributes processes by them, so a provisioned var must never forge or drop one.
  RUN_ATTEMPT_ENV,
  RUN_CLONE_ENV,
  RUN_CLONE_KEY_ENV,
  WORKER_SPAWN_ENV,
]);

/** issue #1783 (R2): the execution attempt the agent CLI runs for. Emitted as
 *  UZI_RUN_ATTEMPT / UZI_RUN_CLONE / UZI_RUN_CLONE_KEY so every process the agent starts
 *  inherits positive attribution to this attempt. */
export interface SdkAttemptEnv {
  /** `<runId>:<attemptId>`. */
  marker: string;
  /** The attempt's absolute clone path. */
  clonePath: string;
  /** `<repoDir>/<key>`. */
  cloneKey: string;
}

/** One mapped gateway model of a provider binding (the wire shape of ClaimProviderModel). */
export interface SdkProviderModel {
  model: string;
  context_window?: number;
}

/** PRD #4320 module 6: the credential the Claude CLI runs under. `oauth` is the vendor subscription
 *  token; `provider` is a frozen gateway binding. Every spawn site builds its env from one of these,
 *  so a provider run can never fall back to an OAuth token by omission. */
export type SdkCredential =
  | { kind: "oauth"; token: string }
  | {
      kind: "provider";
      baseUrl: string;
      authStyle: "bearer" | "x_api_key";
      key: string;
      modelMap: { opus?: SdkProviderModel; sonnet?: SdkProviderModel; haiku: SdkProviderModel; fable?: SdkProviderModel };
    };

/** Exactly the keys the SDK subprocess is allowed to see. The index signature
 *  carries the PRD #18 M3 provisioned tool vars, which are added ONLY from
 *  provision.ts's explicit allowlist (PATH override + NIX_SSL_CERT_FILE /
 *  LOCALE_ARCHIVE) — never a blind merge of devbox's shellenv. */
export interface OAuthSdkEnv {
  CLAUDE_CODE_OAUTH_TOKEN: string;
  HOME: string;
  PATH: string | undefined;
  ANTHROPIC_API_KEY: undefined;
  ANTHROPIC_AUTH_TOKEN: undefined;
  [key: string]: string | undefined;
}

/** The provider-mode env: no OAuth token, the gateway base URL and exactly one auth key. */
export interface ProviderSdkEnv {
  CLAUDE_CODE_OAUTH_TOKEN: undefined;
  ANTHROPIC_BASE_URL: string;
  HOME: string;
  PATH: string | undefined;
  ANTHROPIC_API_KEY: string | undefined;
  ANTHROPIC_AUTH_TOKEN: string | undefined;
  [key: string]: string | undefined;
}

export type SdkEnv = OAuthSdkEnv | ProviderSdkEnv;

const MODEL_ALIAS_ENV = {
  opus: "ANTHROPIC_DEFAULT_OPUS_MODEL",
  sonnet: "ANTHROPIC_DEFAULT_SONNET_MODEL",
  haiku: "ANTHROPIC_DEFAULT_HAIKU_MODEL",
  fable: "ANTHROPIC_DEFAULT_FABLE_MODEL",
} as const;

/** The window a provider credential gets when none of its mapped models configures one. Owner
 *  decision 2026-11-22 (#4611, PRD #4320 Decision Log): the CLI (from 2.1.323 per its changelog;
 *  measured at 2.1.325) assumes 1M tokens for a model id it does not recognize; uzi keeps a
 *  conservative 200K so such a provider compacts early instead of overrunning a gateway's real limit.
 *  The env value always carries the smallest explicit window ({@link smallestContextWindow}), but the
 *  measured CLI ignores it (and this fallback) for ids it recognizes, such as claude-sonnet-4-5 and
 *  us.anthropic.claude-sonnet-4-5 (provider-context-window.integration.test.ts). A partial map (some slots configured,
 *  others not) uses the smallest explicit window for the whole process: a documented limitation,
 *  because CLAUDE_CODE_MAX_CONTEXT_TOKENS cannot differ per model. */
export const PROVIDER_FALLBACK_CONTEXT_WINDOW = 200_000;

/** The smallest `context_window` among ALL mapped models, or undefined when none is set (PRD #4320
 *  Decision Log: CLAUDE_CODE_MAX_CONTEXT_TOKENS is process-wide, so every mapped slot counts). */
function smallestContextWindow(modelMap: Extract<SdkCredential, { kind: "provider" }>["modelMap"]): number | undefined {
  let min: number | undefined;
  for (const m of Object.values(modelMap)) {
    const w = m?.context_window;
    if (typeof w === "number" && (min === undefined || w < min)) min = w;
  }
  return min;
}

/** The credential-specific keys of {@link buildSdkEnv}, in the order the OAuth env always had them. */
function credentialEnv(credential: SdkCredential): Record<string, string | undefined> {
  if (credential.kind === "oauth") {
    return { CLAUDE_CODE_OAUTH_TOKEN: credential.token, ANTHROPIC_API_KEY: undefined, ANTHROPIC_AUTH_TOKEN: undefined };
  }
  const env: Record<string, string | undefined> = {
    CLAUDE_CODE_OAUTH_TOKEN: undefined,
    ANTHROPIC_BASE_URL: credential.baseUrl,
    ANTHROPIC_API_KEY: credential.authStyle === "x_api_key" ? credential.key : undefined,
    ANTHROPIC_AUTH_TOKEN: credential.authStyle === "bearer" ? credential.key : undefined,
  };
  for (const [alias, key] of Object.entries(MODEL_ALIAS_ENV)) {
    env[key] = credential.modelMap[alias as keyof typeof MODEL_ALIAS_ENV]?.model;
  }
  env.ANTHROPIC_SMALL_FAST_MODEL = credential.modelMap.haiku.model;
  env.CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC = "1";
  env.CLAUDE_CODE_MAX_CONTEXT_TOKENS = String(smallestContextWindow(credential.modelMap) ?? PROVIDER_FALLBACK_CONTEXT_WINDOW);
  return env;
}

/** The SDK env before the run's variables fold in: the credential, HOME, the pinned keys and the tool env. */
function sdkPreFoldEnv(
  credential: SdkCredential,
  homeDir: string,
  toolEnv: Record<string, string>,
  runTmpdir: string | undefined,
): SdkEnv {
  const env = {
    ...credentialEnv(credential),
    HOME: homeDir,
    CLAUDE_CONFIG_DIR: claudeConfigDir(homeDir),
    // The RUNNER PATH (PRD #51 M4): the /nix-bearing image PATH under the split (the
    // agent's Bash resolves git/bash/coreutils + provisioned tools), NOT the worker's
    // stripped PATH. Single-uid (#58): the entrypoint exports the same image PATH there
    // (issue #120) — before that this fell back to the worker's npm-mutated PATH, on which
    // /app/node_modules/.bin shadowed the /usr/local/bin agent-browser shim and browser
    // launches lost --no-sandbox. A provisioned toolEnv.PATH still REPLACES this below —
    // but it is built ON this value (provision.ts:126 hands runnerPath() to devbox, :202
    // resolves the $PATH back-ref against it), so it PREPENDS tool bins to the image PATH
    // rather than substituting a foreign one.
    PATH: runnerPath(),
    // --no-sandbox is MANDATORY under the PRD #51 hardening: the browser runs as an
    // unprivileged uid with cap_drop:ALL + no-new-privileges, which makes Chromium's
    // setuid sandbox impossible, so it must be disabled or the launch fails. Putting the
    // flag in the AGENT env (not only the baked /usr/local/bin shim) makes the guarantee
    // survive a shim BYPASS — the real agent-browser CLI reads AGENT_BROWSER_ARGS from its
    // environment (templates/base/Dockerfile:225), so whichever agent-browser binary
    // resolves (the real npm CLI by absolute path, a provisioned same-name binary, a
    // foreign runner-child PATH) inherits it. This is a DELIBERATE, SCOPED reversal of
    // PRD #87 M4, which kept AGENT_BROWSER_* shim-only to keep the agent env minimal: this
    // key crosses the boundary because it is a non-secret launch FLAG (not a
    // credential or a path into the worker) — the same reasoning that added DOCKER_HOST
    // below. Issue #3907 narrowed the rule: the two discovery paths
    // (AGENT_BROWSER_EXECUTABLE_PATH, CHROME_PATH) also cross, set after the toolEnv fold
    // to the uzi-chromium wrapper. FONTCONFIG / XDG / idle-timeout vars STAY in the
    // shim and the wrapper.
    AGENT_BROWSER_ARGS: "--no-sandbox,--disable-dev-shm-usage",
  } as unknown as SdkEnv;
  // 5-bis: the agent's scratch on the runner's private 0700 TMPDIR under the split.
  const tmp = runTmpdir ?? runnerTmpdir();
  if (tmp) env.TMPDIR = tmp;
  if (process.env.UZI_SQLC_DIR) env.UZI_SQLC_DIR = process.env.UZI_SQLC_DIR;
  // Fold in the provisioned tool env. Only allowlisted keys reach here; PATH (if
  // present) is devbox's PATH with the tool bins prepended, so it replaces the
  // inherited one. Never let a provisioned var overwrite a PROTECTED key: the
  // credential, HOME, and the ANTHROPIC_* keys we deliberately pin to undefined so
  // they can't outrank the OAuth token — even if the provision allowlist ever
  // drifts, this second layer keeps the invariant.
  for (const [k, v] of Object.entries(toolEnv)) {
    if (PROTECTED_ENV_KEYS.has(k)) continue;
    env[k] = v;
  }
  return env;
}

/** The refusal text for a run that carries user variables on a worker without the uid split. */
export const REASON_RUN_ENV_NEEDS_SPLIT = "run environment variables need the protected two-uid worker";

/** Refuse (before anything starts) a run that carries user variables unless the uid split is active:
 *  with one uid the CLI's config directory gives no protection (claude-global-config.ts). */
export function assertRunEnvDeliverable(vars: readonly unknown[] | undefined): void {
  if (vars !== undefined && vars.length > 0 && !uidSplitActive()) throw new TrustedExecutionRefusal(REASON_RUN_ENV_NEEDS_SPLIT);
}

/** The keys buildSdkEnv refuses to let a user variable take (the credential, HOME and the pinned ANTHROPIC_* keys). */
const isSdkRunEnvProtected = (k: string): boolean => PROTECTED_ENV_KEYS.has(k);

/** PRD #4416 M2: which of `vars` {@link buildSdkEnv} injects and which it drops for these inputs, computed over
 *  the same pre-fold env and with the same predicate as the builder's fold. The executor names the set once
 *  per run (before any prompt) and passes it to the builder as `RunEnvInjection.set`, which folds exactly it.
 *  Pass the `credential`, `homeDir`, `toolEnv` and `runTmpdir` the builder is given. */
export function sdkRunEnvInjected(
  vars: readonly ClaimRunEnvVar[],
  credential: SdkCredential,
  homeDir: string,
  toolEnv: Record<string, string>,
  runTmpdir?: string,
): RunEnvInjectedSet {
  return runEnvInjectedSet(vars, Object.keys(sdkPreFoldEnv(credential, homeDir, toolEnv, runTmpdir)), isSdkRunEnvProtected);
}

/**
 * Build the sparse env handed to `query({ options: { env } })`.
 *
 * @param credential the OAuth token (CLAUDE_CODE_OAUTH_TOKEN) or the frozen provider binding (PRD #4320)
 * @param homeDir    the pinned HOME (a dir on $UZI_DATA_DIR) for session persistence
 * @param toolEnv    PRD #18 M3: allowlisted vars from devbox provisioning. Already
 *                   filtered by provision.ts (only PROVISION_ENV_ALLOWLIST keys),
 *                   so widening the agent env stays deliberate. PATH here REPLACES
 *                   the inherited PATH (devbox's PATH already prepends tool bins to
 *                   it). Empty ⇒ today's behavior exactly.
 * @param dockerHost PRD #83 Q3/Q4: the resolved DOCKER_HOST for a WIRED worker (the
 *                   keystone resolver's output). When set, the agent's docker CLI
 *                   reaches the sidecar daemon through it. Undefined ⇒ the key is
 *                   omitted entirely, so a sidecar-less worker's docker fails loudly
 *                   ("cannot connect to the Docker daemon") rather than reaching for a
 *                   host socket. Widening the deliberately-minimal agent env is safe:
 *                   it is a socket path/URL, NOT a secret, and is present only when a
 *                   sidecar is wired.
 * @param attempt    issue #1783 (R2): the execution attempt marker (see
 *                   {@link SdkAttemptEnv}). Undefined ⇒ no marker keys are emitted.
 * @param goWorkDir  issue #3870: the managed Go work dir (`RunContext.managedGoWorkDir`).
 *                   Set as GOTMPDIR AFTER the toolEnv fold and the Go concurrency env, so
 *                   a provisioned GOTMPDIR cannot override it. Undefined ⇒ no key from
 *                   this parameter (a provisioned GOTMPDIR behaves as before).
 * @param runTmpdir  issue #4420: the run-owned scratch leaf (`RunContext.runTmpdir`). When set it
 *                   is the TMPDIR instead of the shared runner TMPDIR; undefined ⇒ unchanged.
 * @param runEnv     PRD #4416 M2: the run's user variables. Lead-lane spawn sites only. Folded in
 *                   right after the toolEnv fold with the lowest precedence (see foldRunEnv); every
 *                   worker-set key below still wins; its `set`, when given, is the precomputed
 *                   list the fold applies (see {@link sdkRunEnvInjected}). Undefined ⇒ the env is
 *                   byte-identical to before.
 */
export function buildSdkEnv(
  credential: SdkCredential,
  homeDir: string,
  toolEnv: Record<string, string> = {},
  dockerHost?: string,
  attempt?: SdkAttemptEnv,
  goWorkDir?: string,
  runTmpdir?: string,
  runEnv?: RunEnvInjection,
): SdkEnv {
  assertRunEnvDeliverable(runEnv?.vars);
  const env = sdkPreFoldEnv(credential, homeDir, toolEnv, runTmpdir);
  // PRD #4416 M2: user variables, after the tool profile and before every worker-set key below.
  foldRunEnv(env, runEnv, isSdkRunEnvProtected);
  // PRD #4416 M2: every worker-set key assigned from here on overwrites a user variable of the same
  // name silently (no drop event), which is safe only because each such key is also denied by name
  // (RUN_ENV_DENIED_NAMES / prefixes), so a user variable never reaches this point under it.
  // Issue #3382: the worker's optional Go build concurrency defaults (GOFLAGS=-p=N,
  // GOMAXPROCS=N). Worker-set, applied AFTER the fold so a provisioned var cannot
  // override them; absent settings add no key at all. Not protected keys.
  Object.assign(env, goConcurrencyEnv());
  // Issue #3870: the managed Go work dir, last among the Go keys so nothing provisioned wins.
  if (goWorkDir) env.GOTMPDIR = goWorkDir;
  // DOCKER_HOST is worker-set (not agent-set) and NOT in PROTECTED_ENV_KEYS: the
  // provision allowlist never emits it, so the fold above cannot carry it. Set it AFTER
  // the fold so a provisioned var can never clobber the worker's wired endpoint.
  if (dockerHost) env.DOCKER_HOST = dockerHost;
  // Issue #3907: browser discovery points at the uzi-chromium wrapper, set AFTER the fold
  // (and protected above) so a provisioned var can neither redirect nor drop it.
  Object.assign(env, BROWSER_DISCOVERY_ENV);
  // The operator's extra CA (UZI_CLI_CA_FILE, set by the controller only when
  // workers.extraCA is configured and the uid split is off) reaches the CLI, so a
  // private-CA host it calls (an Anthropic-compatible gateway) verifies. A path to public
  // CA material, not a secret. Set AFTER the fold (and protected above).
  if (process.env.UZI_CLI_CA_FILE) env.NODE_EXTRA_CA_CERTS = process.env.UZI_CLI_CA_FILE;
  // issue #1783 (R2): the attempt marker, set AFTER the fold (and protected above) so a
  // provisioned var can neither forge nor clear it. Absent attempt ⇒ no key at all.
  if (attempt) {
    env[RUN_ATTEMPT_ENV] = attempt.marker;
    env[RUN_CLONE_ENV] = attempt.clonePath;
    env[RUN_CLONE_KEY_ENV] = attempt.cloneKey;
  }
  return env;
}

// ─── Check subprocess environment (M9 + audit, security load-bearing) ─────────
// The checks below (and the js-deps install, PRD #121) execute AGENT-AUTHORED code as
// the WORKER uid — worktree test files, package.json scripts, vite/tsc/go test —
// ENTIRELY OUTSIDE the SDK hook system (guardrails.ts constrains only the AGENT's
// Bash, not a worker-spawned execFile child). The worker process holds the decrypted
// forge PAT + Anthropic token, and its env carries the join token
// (UZI_WORKER_TOKEN[_FILE]) + UZI_API_URL. So a check subprocess gets a SCRUBBED
// REPLACEMENT env — the same discipline provision.ts uses for nix build hooks — never
// a process.env spread: the join-token/API vars are ABSENT BY CONSTRUCTION, so
// agent-authored code cannot read them to impersonate the worker
// (join token → /api/worker/runs/claim → bot forge PAT + the user's Anthropic token).
//
// CLOSED for the local path (PRD #51 M4): the check + `npm ci` subprocesses now run
// under the cap-less `runner` uid (runnerCommand, below), and the join-token FILE at
// /run/secrets/worker_token is 0400 worker-owned, so agent-authored test code — even
// though it executes model-written code the SDK hook system never sees — CANNOT read
// the worker's token at all. The install still runs with --ignore-scripts (js-deps.ts,
// every package manager) as a defense-in-depth REDUCTION of the lifecycle-script
// code-exec path. What the
// same-uid residual used to expose (join token → claim → bot forge PAT + the user's
// Anthropic token) is no longer reachable by these checks on the A1 (root-started) path.
// On a #58 single-uid (non-root) start there is no split and the checks run as the sole
// uid (that PRD's accepted posture); the cross-container k8s form is mapped in
// docs/proc-hardening.md. (This was the same residual class provision.ts documented for
// build hooks; both are closed together by the M4 spawn-as-runner.)

// buildCheckEnv is the scrubbed replacement env for a check subprocess. PATH comes
// from the provisioned toolEnv when present (so go/vitest/tsc resolve), else the
// worker's base PATH; HOME is a writable per-run dir (npm cache/config). Only what the
// toolchains + npm-over-HTTPS demonstrably need is added — never a worker secret.
export function buildCheckEnv(
  source: NodeJS.ProcessEnv,
  homeDir: string,
  toolEnv?: Record<string, string>,
  goWorkDir?: string,
  runTmpdir?: string,
): NodeJS.ProcessEnv {
  const env: NodeJS.ProcessEnv = {
    // The RUNNER PATH (PRD #51 M4): the provisioned toolchain PATH when present, else
    // the /nix-bearing runner image PATH under the split (checks run as `runner`), NOT
    // the worker's stripped PATH. Single-uid (#58): ALSO the image PATH since PRD #120 —
    // the entrypoint now pins UZI_RUNNER_PATH on both branches, so this no longer inherits
    // npm's run-script injections (/app/node_modules/.bin et al).
    PATH: toolEnv?.PATH ?? runnerPath(source),
    HOME: homeDir,
    // No interactive prompt if a check shells out to git; not a secret.
    GIT_TERMINAL_PROMPT: "0",
  };
  // 5-bis: check scratch on the runner's private 0700 TMPDIR under the split.
  const tmp = runTmpdir ?? runnerTmpdir(source);
  if (tmp) env.TMPDIR = tmp;
  if (source.UZI_SQLC_DIR) env.UZI_SQLC_DIR = source.UZI_SQLC_DIR;
  // TLS trust + locale so nix-provided toolchains and `npm ci` over HTTPS work. Prefer
  // the provisioned value, fall back to the image's; never invent, never carry else.
  for (const k of ["NIX_SSL_CERT_FILE", "SSL_CERT_FILE", "LOCALE_ARCHIVE"] as const) {
    const v = toolEnv?.[k] ?? source[k];
    if (v) env[k] = v;
  }
  // Issue #3382: the worker's optional Go build concurrency defaults reach checks too.
  Object.assign(env, goConcurrencyEnv());
  // Issue #3870: the managed Go work dir (absent ⇒ no key; buildCheckEnv never carries a provisioned one).
  if (goWorkDir) env.GOTMPDIR = goWorkDir;
  return env;
}

// The dependency install that made the npm checks runnable used to live here as
// `prepareCheckDeps` (a hardcoded `npm ci --ignore-scripts` in `["web", "agent"]`).
// PRD #121 M1/M2 generalized it into js-deps.ts — lockfile-driven, bounded discovery,
// same sandbox and same flags — and both call sites (the executor's pre-plan install and
// the self-improve check prep in runner.ts) now use that one implementation. It was
// DELETED rather than left as a shim: two install paths for the same job is exactly the
// drift this PRD set out to remove, and the hardcoded dir list was the bug.
