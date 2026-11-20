// Agent-side copy of the run-env name rules (PRD 4416, "Name rules (shared fixture)").
// The api enforces the same rules in api/internal/runenv/names.go; both copies mirror
// fixtures/run-env/name-rules.json, and test/run-env-names.test.ts fails when this
// copy and the fixture differ.

/** Harness versions the lists below were derived from. A bump of either harness must
 *  update the fixture, which test/run-env-names.test.ts enforces. */
export const RUN_ENV_HARNESS_VERSIONS = {
  claude_agent_sdk: "0.3.319",
  codex: "0.167.0",
} as const;

/** One entry of the shared fixture: the denied value, the worker invariant it protects
 *  and a short note on why. */
export interface RunEnvDeniedRule {
  readonly value: string;
  readonly invariant: string;
  readonly note: string;
}

/** Exact names a variable may not use. */
export const RUN_ENV_DENIED_NAMES: readonly RunEnvDeniedRule[] = [
  { value: "HOME", invariant: "managed_dirs", note: "directory the worker sets for the run home or temp space" },
  { value: "PATH", invariant: "process_loader", note: "search path the worker controls for every command" },
  { value: "TMPDIR", invariant: "managed_dirs", note: "directory the worker sets for the run home or temp space" },
  { value: "LANG", invariant: "shell_semantics", note: "locale and shell start-up behaviour the worker fixes" },
  { value: "SHELL", invariant: "shell_semantics", note: "locale and shell start-up behaviour the worker fixes" },
  { value: "ENV", invariant: "shell_semantics", note: "locale and shell start-up behaviour the worker fixes" },
  { value: "IFS", invariant: "shell_semantics", note: "locale and shell start-up behaviour the worker fixes" },
  { value: "PS4", invariant: "shell_semantics", note: "locale and shell start-up behaviour the worker fixes" },
  { value: "PROMPT_COMMAND", invariant: "shell_semantics", note: "locale and shell start-up behaviour the worker fixes" },
  { value: "SHELLOPTS", invariant: "shell_semantics", note: "locale and shell start-up behaviour the worker fixes" },
  { value: "BASHOPTS", invariant: "shell_semantics", note: "locale and shell start-up behaviour the worker fixes" },
  { value: "DOCKER_HOST", invariant: "network_wiring", note: "the worker reaches its Docker daemon through these" },
  { value: "DOCKER_CONFIG", invariant: "managed_dirs", note: "the worker owns the Docker client configuration directory" },
  { value: "DOCKER_CONTEXT", invariant: "network_wiring", note: "the worker reaches its Docker daemon through these" },
  { value: "DOCKER_TLS_VERIFY", invariant: "network_wiring", note: "the worker reaches its Docker daemon through these" },
  { value: "DOCKER_CERT_PATH", invariant: "network_wiring", note: "the worker reaches its Docker daemon through these" },
  { value: "BUILDKIT_HOST", invariant: "network_wiring", note: "the worker reaches its Docker daemon through these" },
  { value: "GOMAXPROCS", invariant: "process_loader", note: "Go toolchain selection and resource limits the worker fixes" },
  { value: "GOFLAGS", invariant: "process_loader", note: "Go toolchain selection and resource limits the worker fixes" },
  { value: "GOTMPDIR", invariant: "managed_dirs", note: "Go build and module directories the worker places" },
  { value: "GOMODCACHE", invariant: "managed_dirs", note: "Go build and module directories the worker places" },
  { value: "GOCACHE", invariant: "managed_dirs", note: "Go build and module directories the worker places" },
  { value: "GOPROXY", invariant: "network_wiring", note: "Go module proxy and checksum database routing" },
  { value: "GOSUMDB", invariant: "network_wiring", note: "Go module proxy and checksum database routing" },
  { value: "GOENV", invariant: "process_loader", note: "Go toolchain selection and resource limits the worker fixes" },
  { value: "GOTOOLCHAIN", invariant: "process_loader", note: "Go toolchain selection and resource limits the worker fixes" },
  { value: "NPM_CONFIG_CACHE", invariant: "managed_dirs", note: "directory the worker sets for the run home or temp space" },
  { value: "npm_config_cache", invariant: "managed_dirs", note: "directory the worker sets for the run home or temp space" },
  { value: "NODE_OPTIONS", invariant: "process_loader", note: "Node start-up options and module search path" },
  { value: "NODE_PATH", invariant: "process_loader", note: "Node start-up options and module search path" },
  { value: "NODE_EXTRA_CA_CERTS", invariant: "network_wiring", note: "Node trust store and TLS verification" },
  { value: "NODE_TLS_REJECT_UNAUTHORIZED", invariant: "network_wiring", note: "Node trust store and TLS verification" },
  { value: "JAVA_TOOL_OPTIONS", invariant: "process_loader", note: "JVM and Ruby start-up options injected into every process" },
  { value: "_JAVA_OPTIONS", invariant: "process_loader", note: "JVM and Ruby start-up options injected into every process" },
  { value: "JDK_JAVA_OPTIONS", invariant: "process_loader", note: "JVM and Ruby start-up options injected into every process" },
  { value: "CURL_HOME", invariant: "network_wiring", note: "client configuration and credential files for curl and wget" },
  { value: "NETRC", invariant: "network_wiring", note: "client configuration and credential files for curl and wget" },
  { value: "WGETRC", invariant: "network_wiring", note: "client configuration and credential files for curl and wget" },
  { value: "CURL_CA_BUNDLE", invariant: "network_wiring", note: "trust store of curl and Python requests" },
  { value: "REQUESTS_CA_BUNDLE", invariant: "network_wiring", note: "trust store of curl and Python requests" },
  { value: "RUBYOPT", invariant: "process_loader", note: "JVM and Ruby start-up options injected into every process" },
  { value: "AGENT_BROWSER_ARGS", invariant: "process_loader", note: "browser discovery for the agent browser tool" },
  { value: "AGENT_BROWSER_EXECUTABLE_PATH", invariant: "process_loader", note: "browser discovery for the agent browser tool" },
  { value: "CHROME_PATH", invariant: "process_loader", note: "browser discovery for the agent browser tool" },
  { value: "CLAUDE_CODE_OAUTH_TOKEN", invariant: "provider_auth", note: "credential the worker selects for the model provider" },
  { value: "ANTHROPIC_API_KEY", invariant: "provider_auth", note: "credential the worker selects for the model provider" },
  { value: "ANTHROPIC_AUTH_TOKEN", invariant: "provider_auth", note: "credential the worker selects for the model provider" },
  { value: "UZI_SQLC_DIR", invariant: "managed_dirs", note: "run-attempt and clone markers the worker sets" },
  { value: "UZI_RUN_ATTEMPT", invariant: "managed_dirs", note: "run-attempt and clone markers the worker sets" },
  { value: "UZI_RUN_CLONE", invariant: "managed_dirs", note: "run-attempt and clone markers the worker sets" },
  { value: "UZI_RUN_CLONE_KEY", invariant: "managed_dirs", note: "run-attempt and clone markers the worker sets" },
  { value: "UZI_WORKER_SPAWN", invariant: "worker_credentials", note: "marker of a worker-spawned process" },
  { value: "GIT_CONFIG_COUNT", invariant: "git_authority", note: "git configuration injected through the environment" },
  { value: "GIT_CONFIG_PARAMETERS", invariant: "git_authority", note: "git configuration injected through the environment" },
  { value: "API_TIMEOUT_MS", invariant: "harness_control", note: "Claude CLI request timeout" },
  { value: "MAX_THINKING_TOKENS", invariant: "harness_control", note: "Claude CLI thinking budget" },
  { value: "MAX_MCP_OUTPUT_TOKENS", invariant: "harness_control", note: "Claude CLI tool output bound" },
  { value: "API_FORCE_IDLE_TIMEOUT", invariant: "harness_control", note: "Claude CLI idle-timeout override read by the pinned binary" },
  { value: "ENABLE_TOOL_SEARCH", invariant: "harness_control", note: "Claude CLI feature switch (tool search) read by the pinned binary" },
  { value: "ENABLE_ENHANCED_TELEMETRY_BETA", invariant: "harness_control", note: "Claude CLI telemetry switch read by the pinned binary" },
  { value: "ENABLE_CLAUDEAI_MCP_SERVERS", invariant: "harness_control", note: "Claude CLI switch for claude.ai MCP servers read by the pinned binary" },
  { value: "SLASH_COMMAND_TOOL_CHAR_BUDGET", invariant: "harness_control", note: "Claude CLI slash-command budget read by the pinned binary" },
  { value: "FORCE_CODE_TERMINAL", invariant: "harness_control", note: "Claude CLI terminal mode read by the pinned binary" },
  { value: "IS_SANDBOX", invariant: "harness_control", note: "Claude CLI sandbox-mode assertion read by the pinned binary" },
  { value: "SESSION_INGRESS_URL", invariant: "network_wiring", note: "Claude CLI session ingress endpoint read by the pinned binary" },
  { value: "BETA_TRACING_ENDPOINT", invariant: "network_wiring", note: "Claude CLI tracing endpoint read by the pinned binary" },
  { value: "SDK_NATIVE_BIN", invariant: "process_loader", note: "path of the native Claude binary the SDK launches" },
  { value: "AUTOMODE_DECISION_LOG", invariant: "harness_control", note: "Claude CLI auto-mode decision log path read by the pinned binary" },
  { value: "NODE_DEBUG", invariant: "harness_control", note: "Node debug log selector read by the Claude CLI runtime" },
  { value: "NODE_V8_COVERAGE", invariant: "process_loader", note: "Node coverage output directory read by the Claude CLI runtime" },
  { value: "NODE_REDIRECT_WARNINGS", invariant: "harness_control", note: "Node warning redirection file read by the Claude CLI runtime" },
  { value: "NODE_ICU_DATA", invariant: "process_loader", note: "Node ICU data path read by the Claude CLI runtime" },
  { value: "NODE_USE_ENV_PROXY", invariant: "network_wiring", note: "Node switch that routes built-in HTTP clients through *_PROXY variables, read by the Claude CLI runtime" },
  { value: "NODE_USE_SYSTEM_CA", invariant: "network_wiring", note: "Node switch that trusts the system CA store, read by the Claude CLI runtime" },
  { value: "NODE_COMPILE_CACHE", invariant: "managed_dirs", note: "Node compile-cache directory read by the Claude CLI runtime" },
  { value: "UV_THREADPOOL_SIZE", invariant: "process_loader", note: "libuv thread-pool size read by the Claude CLI runtime" },
  { value: "SSLKEYLOGFILE", invariant: "network_wiring", note: "TLS key log file read by the Node and Bun runtimes; would write session keys to a path the user picks" },
  { value: "NODE_CHANNEL_FD", invariant: "process_loader", note: "Node child-process IPC channel descriptor read at start-up by the Claude CLI runtime" },
  { value: "NODE_UNIQUE_ID", invariant: "process_loader", note: "Node cluster worker marker read at start-up by the Claude CLI runtime" },
  { value: "NODE_CLUSTER_SCHED_POLICY", invariant: "process_loader", note: "Node cluster scheduling policy read at start-up by the Claude CLI runtime" },
  { value: "NODE_TEST_CONTEXT", invariant: "process_loader", note: "Node test-runner child marker read at start-up by the Claude CLI runtime" },
  { value: "NODE_REPL_EXTERNAL_MODULE", invariant: "process_loader", note: "Node REPL module loaded in place of the built-in REPL by the Claude CLI runtime" },
  { value: "GRPC_DEFAULT_SSL_ROOTS_FILE_PATH", invariant: "network_wiring", note: "gRPC TLS root certificate file read by the pinned Claude CLI binary" },
];
export const RUN_ENV_DENIED_PREFIXES: readonly RunEnvDeniedRule[] = [
  { value: "UZI_", invariant: "worker_credentials", note: "worker-owned variables" },
  { value: "CLAUDE_", invariant: "provider_auth", note: "provider selection and credentials; includes every CLAUDE_CODE_USE_* selector" },
  { value: "ANTHROPIC_", invariant: "provider_auth", note: "provider selection and credentials; includes every CLAUDE_CODE_USE_* selector" },
  { value: "CODEX_", invariant: "provider_auth", note: "provider selection and credentials; includes every CLAUDE_CODE_USE_* selector" },
  { value: "OPENAI_", invariant: "provider_auth", note: "provider selection and credentials; includes every CLAUDE_CODE_USE_* selector" },
  { value: "MCP_", invariant: "harness_control", note: "MCP and feature switches read by the Claude CLI" },
  { value: "DISABLE_", invariant: "harness_control", note: "MCP and feature switches read by the Claude CLI" },
  { value: "GH_", invariant: "git_authority", note: "forge CLIs and tokens" },
  { value: "GITHUB_", invariant: "git_authority", note: "forge CLIs and tokens" },
  { value: "GITLAB_", invariant: "git_authority", note: "forge CLIs and tokens" },
  { value: "GLAB_", invariant: "git_authority", note: "forge CLIs and tokens" },
  { value: "GIT_", invariant: "git_authority", note: "git and ssh authority" },
  { value: "SSH_", invariant: "git_authority", note: "git and ssh authority" },
  { value: "LD_", invariant: "process_loader", note: "dynamic loader injection" },
  { value: "DYLD_", invariant: "process_loader", note: "dynamic loader injection" },
  { value: "BASH_", invariant: "shell_semantics", note: "bash behaviour variables" },
  { value: "XDG_", invariant: "managed_dirs", note: "XDG base directories under the run home" },
  { value: "SSL_CERT_", invariant: "network_wiring", note: "OpenSSL trust store" },
  { value: "PYTHON", invariant: "process_loader", note: "interpreter start-up and module search" },
  { value: "PERL5", invariant: "process_loader", note: "interpreter start-up and module search" },
  { value: "NIX_", invariant: "process_loader", note: "toolchain wrappers the worker image provides" },
  { value: "DEVBOX_", invariant: "process_loader", note: "toolchain wrappers the worker image provides" },
  { value: "BUN_", invariant: "process_loader", note: "Bun runtime configuration read by the Claude CLI binary (BUN_CONFIG_*, BUN_INSTALL_*)" },
  { value: "SELF_HOSTED_RUNNER_", invariant: "harness_control", note: "Claude CLI self-hosted runner mode settings (SELF_HOSTED_RUNNER_*), read by the pinned binary" },
  { value: "CCR_", invariant: "harness_control", note: "Claude CLI remote-session variables (CCR_*), read by the pinned binary" },
];
export const RUN_ENV_DENIED_SUFFIXES: readonly RunEnvDeniedRule[] = [
  { value: "_PROXY", invariant: "network_wiring", note: "changes network routing of every process" },
];

/** The shape of a variable name. */
export const RUN_ENV_NAME_REGEXP = /^[A-Z_][A-Z0-9_]{0,127}$/;

/** Returns the rule a name breaks, or null when it is allowed. Same order and
 *  semantics as ValidateName in api/internal/runenv/names.go: shape, then exact
 *  names, then prefixes, then suffixes. */
export function runEnvNameViolation(name: string): string | null {
  if (!RUN_ENV_NAME_REGEXP.test(name)) {
    return "must match ^[A-Z_][A-Z0-9_]{0,127}$ (upper-case letters, digits and underscores, not starting with a digit, at most 128 characters)";
  }
  for (const n of RUN_ENV_DENIED_NAMES) {
    if (name === n.value) return `${name} is set or reserved by the worker (denied name; protects ${n.invariant})`;
  }
  for (const p of RUN_ENV_DENIED_PREFIXES) {
    if (name.startsWith(p.value)) {
      return `denied prefix ${p.value}: names starting with it are reserved for the worker and the agent harness (protects ${p.invariant})`;
    }
  }
  for (const s of RUN_ENV_DENIED_SUFFIXES) {
    if (name.endsWith(s.value)) {
      return `denied suffix ${s.value}: names ending in it change network routing and are reserved (protects ${s.invariant})`;
    }
  }
  return null;
}
