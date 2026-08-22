// Sparse credential env for the Claude Agent SDK subprocess (bottega's
// sparse-env pattern, tightened for uzi's trust boundary).
//
// The SDK `env` option REPLACES the subprocess environment entirely (it is not
// merged with process.env — see the SDK's Options.env docs), so the object we
// build here is exactly what the agent's Bash tool and every child it spawns
// will see. That is the load-bearing property behind the primary directive:
//
//   - The agent gets ONLY the Anthropic OAuth token + HOME + PATH.
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

import { runnerPath, runnerTmpdir } from "./runner-uid.js";

/** Keys a provisioned tool env may NEVER set (PRD #18 M3): the OAuth credential,
 *  HOME, and the two ANTHROPIC_* keys pinned to undefined so nothing outranks the
 *  OAuth token in the SDK's auth precedence. */
const PROTECTED_ENV_KEYS: ReadonlySet<string> = new Set([
  "CLAUDE_CODE_OAUTH_TOKEN",
  "HOME",
  "ANTHROPIC_API_KEY",
  "ANTHROPIC_AUTH_TOKEN",
]);

/** Exactly the keys the SDK subprocess is allowed to see. The index signature
 *  carries the PRD #18 M3 provisioned tool vars, which are added ONLY from
 *  provision.ts's explicit allowlist (PATH override + NIX_SSL_CERT_FILE /
 *  LOCALE_ARCHIVE) — never a blind merge of devbox's shellenv. */
export interface SdkEnv {
  CLAUDE_CODE_OAUTH_TOKEN: string;
  HOME: string;
  PATH: string | undefined;
  ANTHROPIC_API_KEY: undefined;
  ANTHROPIC_AUTH_TOKEN: undefined;
  [key: string]: string | undefined;
}

/**
 * Build the sparse env handed to `query({ options: { env } })`.
 *
 * @param oauthToken the Anthropic subscription OAuth token (CLAUDE_CODE_OAUTH_TOKEN)
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
 */
export function buildSdkEnv(
  oauthToken: string,
  homeDir: string,
  toolEnv: Record<string, string> = {},
  dockerHost?: string,
): SdkEnv {
  const env: SdkEnv = {
    CLAUDE_CODE_OAUTH_TOKEN: oauthToken,
    HOME: homeDir,
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
    ANTHROPIC_API_KEY: undefined,
    ANTHROPIC_AUTH_TOKEN: undefined,
  };
  // 5-bis: the agent's scratch on the runner's private 0700 TMPDIR under the split.
  const tmp = runnerTmpdir();
  if (tmp) env.TMPDIR = tmp;
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
  // DOCKER_HOST is worker-set (not agent-set) and NOT in PROTECTED_ENV_KEYS: the
  // provision allowlist never emits it, so the fold above cannot carry it. Set it AFTER
  // the fold so a provisioned var can never clobber the worker's wired endpoint.
  if (dockerHost) env.DOCKER_HOST = dockerHost;
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
  const tmp = runnerTmpdir(source);
  if (tmp) env.TMPDIR = tmp;
  // TLS trust + locale so nix-provided toolchains and `npm ci` over HTTPS work. Prefer
  // the provisioned value, fall back to the image's; never invent, never carry else.
  for (const k of ["NIX_SSL_CERT_FILE", "SSL_CERT_FILE", "LOCALE_ARCHIVE"] as const) {
    const v = toolEnv?.[k] ?? source[k];
    if (v) env[k] = v;
  }
  return env;
}

// The dependency install that made the npm checks runnable used to live here as
// `prepareCheckDeps` (a hardcoded `npm ci --ignore-scripts` in `["web", "agent"]`).
// PRD #121 M1/M2 generalized it into js-deps.ts — lockfile-driven, bounded discovery,
// same sandbox and same flags — and both call sites (the executor's pre-plan install and
// the self-improve check prep in runner.ts) now use that one implementation. It was
// DELETED rather than left as a shim: two install paths for the same job is exactly the
// drift this PRD set out to remove, and the hardcoded dir list was the bug.
