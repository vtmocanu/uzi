// Agent-side copy of the run-env name rules (PRD 4416, "Name rules (shared fixture)").
// The api enforces the same rules in api/internal/runenv/names.go; both copies mirror
// fixtures/run-env/name-rules.json, and test/run-env-names.test.ts fails when this
// copy and the fixture differ. The fixture also records, for every entry, the worker invariant
// it protects (worker_credentials, provider_auth, harness_control, git_authority, managed_dirs,
// network_wiring, process_loader or shell_semantics) and a note; this copy carries the values only.

/** Harness versions the lists below were derived from. A bump of either harness must
 *  update the fixture, which test/run-env-names.test.ts enforces. */
export const RUN_ENV_HARNESS_VERSIONS = {
  claude_agent_sdk: "0.3.319",
  codex: "0.167.0",
} as const;

/** Exact names a variable may not use. */
export const RUN_ENV_DENIED_NAMES: readonly string[] = [
  "HOME", "PATH", "TMPDIR", "TMP", "TEMP", "LANG", "LANGUAGE", "SHELL", "ENV", "IFS", "PS4", "PROMPT_COMMAND", "SHELLOPTS",
  "BASHOPTS", "CDPATH", "ZDOTDIR", "DOCKER_HOST", "DOCKER_CONFIG", "DOCKER_CONTEXT", "DOCKER_TLS_VERIFY",
  "DOCKER_CERT_PATH", "BUILDKIT_HOST", "GOMAXPROCS", "GOFLAGS", "GOTMPDIR", "GOMODCACHE", "GOCACHE", "GOPROXY",
  "GOSUMDB", "GOENV", "GOTOOLCHAIN", "NPM_CONFIG_CACHE", "npm_config_cache", "NODE_OPTIONS", "NODE_PATH",
  "NODE_EXTRA_CA_CERTS", "NODE_TLS_REJECT_UNAUTHORIZED", "NODE_DEBUG", "NODE_V8_COVERAGE",
  "NODE_REDIRECT_WARNINGS", "NODE_ICU_DATA", "NODE_USE_ENV_PROXY", "NODE_USE_SYSTEM_CA", "NODE_COMPILE_CACHE",
  "NODE_CHANNEL_FD", "UV_THREADPOOL_SIZE", "SSLKEYLOGFILE", "CARES_HOSTS", "HOSTALIASES", "LOCALDOMAIN", "RES_OPTIONS", "OPENSSL_CONF", "OPENSSL_MODULES",
  "OPENSSL_ENGINES", "JAVA_TOOL_OPTIONS", "_JAVA_OPTIONS", "JDK_JAVA_OPTIONS", "CURL_HOME", "NETRC", "WGETRC",
  "CURL_CA_BUNDLE", "REQUESTS_CA_BUNDLE", "RUBYOPT", "AGENT_BROWSER_ARGS", "AGENT_BROWSER_EXECUTABLE_PATH",
  "CHROME_PATH", "CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "UZI_SQLC_DIR",
  "UZI_RUN_ATTEMPT", "UZI_RUN_CLONE", "UZI_RUN_CLONE_KEY", "UZI_WORKER_SPAWN", "GIT_CONFIG_COUNT",
  "GIT_CONFIG_PARAMETERS", "API_TIMEOUT_MS", "API_FORCE_IDLE_TIMEOUT", "MAX_THINKING_TOKENS",
  "MAX_MCP_OUTPUT_TOKENS", "CLAUDECODE", "CLAUBBIT", "IS_SANDBOX", "SDK_NATIVE_BIN", "SESSION_INGRESS_URL",
  "BETA_TRACING_ENDPOINT", "VOICE_STREAM_BASE_URL", "USE_LOCAL_OAUTH", "USE_STAGING_OAUTH", "DEBUG_AUTH",
  "DEBUG_SDK", "DEBUG_CLAUDE_AGENT_SDK", "OTEL_LOG_RAW_API_BODIES", "OTEL_LOG_TOOL_CONTENT",
  "OTEL_LOG_TOOL_DETAILS", "OTEL_LOG_USER_PROMPTS", "OTEL_LOG_ASSISTANT_RESPONSES", "OTEL_LOG_MANAGED_SETTINGS",
  "FORCE_VCR", "VCR_RECORD", "MAX_STRUCTURED_OUTPUT_RETRIES", "FALLBACK_FOR_ALL_PRIMARY_MODELS",
  "FORCE_PROMPT_CACHING_5M", "ENABLE_PROMPT_CACHING_1H", "ENABLE_PROMPT_CACHING_1H_BEDROCK",
  "ENABLE_TOOL_SEARCH", "ENABLE_CLAUDEAI_MCP_SERVERS", "ENABLE_ENHANCED_TELEMETRY_BETA",
  "ENABLE_BETA_TRACING_DETAILED", "ENABLE_LSP_TOOL", "ENABLE_MCP_LARGE_OUTPUT_FILES",
  "ENABLE_SESSION_BACKGROUNDING", "ENABLE_SESSION_PERSISTENCE", "ENABLE_LOCKLESS_UPDATES",
  "ENABLE_PID_BASED_VERSION_LOCKING", "FORCE_AUTOUPDATE_PLUGINS", "TEST_ENABLE_SESSION_PERSISTENCE",
  "SLASH_COMMAND_TOOL_CHAR_BUDGET", "SYSTEM_REMINDER_MEMORY_CONTEXT", "USE_API_CONTEXT_MANAGEMENT",
  "USE_BUILTIN_RIPGREP", "EMBEDDED_SEARCH_TOOLS", "ALLOW_ANT_COMPUTER_USE_MCP", "ULTRAPLAN_PROMPT_FILE",
  "ENVIRONMENT_SERVICE_KEY", "LOCAL_BRIDGE", "VITALS_EMITTER_BIN", "ANT_CLAUDE_CODE_METRICS_ENDPOINT"
];

export const RUN_ENV_DENIED_PREFIXES: readonly string[] = [
  "UZI_", "CLAUDE_", "_CLAUDE_", "ANTHROPIC_", "CODEX_", "OPENAI_", "MCP_", "DISABLE_", "GH_", "GITHUB_", "GITLAB_",
  "GLAB_", "GIT_", "SSH_", "LD_", "DYLD_", "BASH_", "XDG_", "SSL_CERT_", "PYTHON", "PERL5", "NIX_", "DEVBOX_",
  "BUN_", "ANT_OTEL_", "AGENT_PROXY_", "SELF_HOSTED_RUNNER_", "CCR_", "LC_", "MIMALLOC_"
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
