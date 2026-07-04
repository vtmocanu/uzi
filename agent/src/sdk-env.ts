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

/** Exactly the keys the SDK subprocess is allowed to see. */
export interface SdkEnv {
  CLAUDE_CODE_OAUTH_TOKEN: string;
  HOME: string;
  PATH: string | undefined;
  ANTHROPIC_API_KEY: undefined;
  ANTHROPIC_AUTH_TOKEN: undefined;
}

/**
 * Build the sparse env handed to `query({ options: { env } })`.
 *
 * @param oauthToken the Anthropic subscription OAuth token (CLAUDE_CODE_OAUTH_TOKEN)
 * @param homeDir    the pinned HOME (a dir on $UZI_DATA_DIR) for session persistence
 */
export function buildSdkEnv(oauthToken: string, homeDir: string): SdkEnv {
  return {
    CLAUDE_CODE_OAUTH_TOKEN: oauthToken,
    HOME: homeDir,
    // Inherited so the bundled CLI can resolve git/bash/coreutils on PATH.
    PATH: process.env.PATH,
    ANTHROPIC_API_KEY: undefined,
    ANTHROPIC_AUTH_TOKEN: undefined,
  };
}
