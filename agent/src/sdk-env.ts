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
 */
export function buildSdkEnv(oauthToken: string, homeDir: string, toolEnv: Record<string, string> = {}): SdkEnv {
  const env: SdkEnv = {
    CLAUDE_CODE_OAUTH_TOKEN: oauthToken,
    HOME: homeDir,
    // Inherited so the bundled CLI can resolve git/bash/coreutils on PATH.
    PATH: process.env.PATH,
    ANTHROPIC_API_KEY: undefined,
    ANTHROPIC_AUTH_TOKEN: undefined,
  };
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
