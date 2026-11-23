// Agent-side copy of the run-env name rules (PRD 4416, "Name rules (shared fixture)").
// The api enforces the same rules in api/internal/runenv/names.go; both copies mirror
// fixtures/run-env/name-rules.json, and test/run-env-names.test.ts fails when this
// copy and the fixture differ.

/** Harness versions the lists below were derived from. A bump of either harness must
 *  update the fixture, which test/run-env-names.test.ts enforces. */
export const RUN_ENV_HARNESS_VERSIONS = {
  claude_agent_sdk: "0.3.325",
  codex: "0.169.0",
} as const;

/** Exact names a variable may not use. */
export const RUN_ENV_DENIED_NAMES: readonly string[] = [
  "HOME", "PATH", "TMPDIR", "LANG", "SHELL", "ENV", "IFS", "PS4", "PROMPT_COMMAND", "SHELLOPTS", "BASHOPTS",
  "DOCKER_HOST", "DOCKER_CONFIG", "DOCKER_CONTEXT", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH", "BUILDKIT_HOST",
  "GOMAXPROCS", "GOFLAGS", "GOTMPDIR", "GOMODCACHE", "GOCACHE", "GOPROXY", "GOSUMDB", "GOENV", "GOTOOLCHAIN",
  "NPM_CONFIG_CACHE", "npm_config_cache",
  "NODE_OPTIONS", "NODE_PATH", "NODE_EXTRA_CA_CERTS", "NODE_TLS_REJECT_UNAUTHORIZED",
  "JAVA_TOOL_OPTIONS", "_JAVA_OPTIONS", "JDK_JAVA_OPTIONS",
  "CURL_HOME", "NETRC", "WGETRC", "CURL_CA_BUNDLE", "REQUESTS_CA_BUNDLE", "RUBYOPT",
  "AGENT_BROWSER_ARGS", "AGENT_BROWSER_EXECUTABLE_PATH", "CHROME_PATH",
  "CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN",
  "UZI_SQLC_DIR", "UZI_RUN_ATTEMPT", "UZI_RUN_CLONE", "UZI_RUN_CLONE_KEY", "UZI_WORKER_SPAWN",
  "GIT_CONFIG_COUNT", "GIT_CONFIG_PARAMETERS",
  "API_TIMEOUT_MS", "MAX_THINKING_TOKENS", "MAX_MCP_OUTPUT_TOKENS",
  "NODE_INSPECTOR_IPC", "VSCODE_INSPECTOR_OPTIONS", "TEMP", "TMP", "SYSTEMDRIVE",
  "API_FORCE_IDLE_TIMEOUT", "ENABLE_TOOL_SEARCH", "SLASH_COMMAND_TOOL_CHAR_BUDGET", "AUTOMODE_DECISION_LOG",
  "NODE_USE_SYSTEM_CA", "ENABLE_BETA_TRACING_DETAILED", "ENABLE_ENHANCED_TELEMETRY_BETA", "BETA_TRACING_ENDPOINT",
  "_CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL",
];

export const RUN_ENV_DENIED_PREFIXES: readonly string[] = [
  "UZI_", "CLAUDE_", "ANTHROPIC_", "CODEX_", "OPENAI_", "MCP_", "DISABLE_",
  "GH_", "GITHUB_", "GITLAB_", "GLAB_",
  "GIT_", "SSH_",
  "LD_", "DYLD_", "BASH_", "XDG_", "SSL_CERT_", "PYTHON", "PERL5", "NIX_", "DEVBOX_", "BUN_",
];

export const RUN_ENV_DENIED_SUFFIXES: readonly string[] = ["_PROXY"];

/** The shape of a variable name. */
const RUN_ENV_NAME_REGEXP = /^[A-Z_][A-Z0-9_]{0,127}$/;

/** Returns the rule a name breaks, or null when it is allowed. Same order and
 *  semantics as ValidateName in api/internal/runenv/names.go: shape, then exact
 *  names, then prefixes, then suffixes. */
export function runEnvNameViolation(name: string): string | null {
  if (!RUN_ENV_NAME_REGEXP.test(name)) {
    return "must match ^[A-Z_][A-Z0-9_]{0,127}$ (upper-case letters, digits and underscores, not starting with a digit, at most 128 characters)";
  }
  if (RUN_ENV_DENIED_NAMES.includes(name)) {
    return `${name} is set or reserved by the worker (denied name)`;
  }
  for (const p of RUN_ENV_DENIED_PREFIXES) {
    if (name.startsWith(p)) {
      return `denied prefix ${p}: names starting with it are reserved for the worker and the agent harness`;
    }
  }
  for (const s of RUN_ENV_DENIED_SUFFIXES) {
    if (name.endsWith(s)) {
      return `denied suffix ${s}: names ending in it change network routing and are reserved`;
    }
  }
  return null;
}
