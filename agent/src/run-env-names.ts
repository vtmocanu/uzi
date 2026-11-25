// Agent-side copy of the run-env name rules (PRD 4416, "Name rules (shared fixture)").
// The api enforces the same rules in api/internal/runenv/names.go; both copies mirror
// fixtures/run-env/name-rules.json, and test/run-env-names.test.ts fails when this
// copy and the fixture differ.

/** Harness versions the lists below were derived from. A bump of either harness must
 *  update the fixture, which test/run-env-names.test.ts enforces. */
export const RUN_ENV_HARNESS_VERSIONS = {
  claude_agent_sdk: "0.3.325",
  codex: "0.171.0",
} as const;

/** The worker invariants a denylist entry protects (fixtures/run-env/name-rules.json "invariants"). */
export type RunEnvInvariant =
  | "worker_credentials"
  | "provider_auth"
  | "harness_control"
  | "git_authority"
  | "managed_dirs"
  | "network_wiring"
  | "process_loader"
  | "shell_semantics";

/** One denylist entry. Only `value` takes part in matching. */
export interface RunEnvDeniedRule {
  readonly value: string;
  readonly invariant: RunEnvInvariant;
  readonly note: string;
}

/** Exact names a variable may not use. */
export const RUN_ENV_DENIED_NAMES: readonly RunEnvDeniedRule[] = [
  { value: "HOME", invariant: "managed_dirs", note: "home directory the worker manages for the run" },
  { value: "PATH", invariant: "process_loader", note: "command lookup path the worker controls" },
  { value: "TMPDIR", invariant: "managed_dirs", note: "temporary directory the worker manages" },
  { value: "LANG", invariant: "shell_semantics", note: "locale of the commands the worker runs" },
  { value: "SHELL", invariant: "shell_semantics", note: "shell the agent harness spawns" },
  { value: "ENV", invariant: "shell_semantics", note: "POSIX shell startup file" },
  { value: "IFS", invariant: "shell_semantics", note: "shell field separator" },
  { value: "PS4", invariant: "shell_semantics", note: "shell trace prompt, expanded by the shell" },
  { value: "PROMPT_COMMAND", invariant: "shell_semantics", note: "bash command run before each prompt" },
  { value: "SHELLOPTS", invariant: "shell_semantics", note: "bash options applied to every shell" },
  { value: "BASHOPTS", invariant: "shell_semantics", note: "bash options applied to every shell" },
  { value: "DOCKER_HOST", invariant: "network_wiring", note: "Docker endpoint the worker provisions" },
  { value: "DOCKER_CONFIG", invariant: "network_wiring", note: "Docker client config and credentials the worker provisions" },
  { value: "DOCKER_CONTEXT", invariant: "network_wiring", note: "Docker endpoint selection" },
  { value: "DOCKER_TLS_VERIFY", invariant: "network_wiring", note: "Docker endpoint TLS wiring" },
  { value: "DOCKER_CERT_PATH", invariant: "network_wiring", note: "Docker endpoint TLS wiring" },
  { value: "BUILDKIT_HOST", invariant: "network_wiring", note: "BuildKit endpoint the worker provisions" },
  { value: "GOMAXPROCS", invariant: "harness_control", note: "CPU budget the worker sets for Go processes" },
  { value: "GOFLAGS", invariant: "harness_control", note: "Go build flags the worker sets" },
  { value: "GOTMPDIR", invariant: "managed_dirs", note: "Go temporary directory the worker manages" },
  { value: "GOMODCACHE", invariant: "managed_dirs", note: "Go module cache the worker manages" },
  { value: "GOCACHE", invariant: "managed_dirs", note: "Go build cache the worker manages" },
  { value: "GOPROXY", invariant: "network_wiring", note: "Go module download routing" },
  { value: "GOSUMDB", invariant: "network_wiring", note: "Go checksum database routing" },
  { value: "GOENV", invariant: "managed_dirs", note: "Go environment file location" },
  { value: "GOTOOLCHAIN", invariant: "process_loader", note: "Go toolchain selection and download" },
  { value: "NPM_CONFIG_CACHE", invariant: "managed_dirs", note: "npm cache directory the worker manages" },
  { value: "npm_config_cache", invariant: "managed_dirs", note: "npm cache directory the worker manages" },
  { value: "NODE_OPTIONS", invariant: "process_loader", note: "Node runtime flags applied to every Node process, harness included" },
  { value: "NODE_PATH", invariant: "process_loader", note: "Node module resolution path" },
  { value: "NODE_EXTRA_CA_CERTS", invariant: "network_wiring", note: "extra trust roots for every Node process" },
  { value: "NODE_TLS_REJECT_UNAUTHORIZED", invariant: "network_wiring", note: "disables TLS verification for every Node process" },
  { value: "JAVA_TOOL_OPTIONS", invariant: "process_loader", note: "JVM flags applied to every JVM" },
  { value: "_JAVA_OPTIONS", invariant: "process_loader", note: "JVM flags applied to every JVM" },
  { value: "JDK_JAVA_OPTIONS", invariant: "process_loader", note: "JVM flags applied by the java launcher" },
  { value: "CURL_HOME", invariant: "managed_dirs", note: "curl config directory" },
  { value: "NETRC", invariant: "git_authority", note: "netrc file carries forge credentials" },
  { value: "WGETRC", invariant: "network_wiring", note: "wget config can set proxies and certificates" },
  { value: "CURL_CA_BUNDLE", invariant: "network_wiring", note: "trust roots for curl" },
  { value: "REQUESTS_CA_BUNDLE", invariant: "network_wiring", note: "trust roots for Python requests" },
  { value: "RUBYOPT", invariant: "process_loader", note: "Ruby flags applied to every Ruby process" },
  { value: "AGENT_BROWSER_ARGS", invariant: "harness_control", note: "browser launch arguments of the agent browser tool" },
  { value: "AGENT_BROWSER_EXECUTABLE_PATH", invariant: "harness_control", note: "browser binary of the agent browser tool" },
  { value: "CHROME_PATH", invariant: "harness_control", note: "browser binary used by the agent browser tool" },
  { value: "CLAUDE_CODE_OAUTH_TOKEN", invariant: "worker_credentials", note: "Anthropic subscription token the worker injects" },
  { value: "ANTHROPIC_API_KEY", invariant: "provider_auth", note: "Anthropic API credential" },
  { value: "ANTHROPIC_AUTH_TOKEN", invariant: "provider_auth", note: "Anthropic gateway credential" },
  { value: "UZI_SQLC_DIR", invariant: "harness_control", note: "worker-managed sqlc location" },
  { value: "UZI_RUN_ATTEMPT", invariant: "harness_control", note: "worker-managed run attempt identity" },
  { value: "UZI_RUN_CLONE", invariant: "harness_control", note: "worker-managed run clone path" },
  { value: "UZI_RUN_CLONE_KEY", invariant: "harness_control", note: "worker-managed run clone identity" },
  { value: "UZI_WORKER_SPAWN", invariant: "harness_control", note: "worker-managed spawn marker" },
  { value: "GIT_CONFIG_COUNT", invariant: "git_authority", note: "git config injection" },
  { value: "GIT_CONFIG_PARAMETERS", invariant: "git_authority", note: "git config injection" },
  { value: "API_TIMEOUT_MS", invariant: "harness_control", note: "request timeout of the Claude CLI" },
  { value: "MAX_THINKING_TOKENS", invariant: "harness_control", note: "thinking budget of the Claude CLI" },
  { value: "MAX_MCP_OUTPUT_TOKENS", invariant: "harness_control", note: "tool output bound of the Claude CLI" },
  { value: "NODE_REPL_EXTERNAL_MODULE", invariant: "process_loader", note: "Node or OpenSSL runtime loader or startup control" },
  { value: "NODE_CHANNEL_FD", invariant: "process_loader", note: "Node or OpenSSL runtime loader or startup control" },
  { value: "NODE_CHANNEL_SERIALIZATION_MODE", invariant: "process_loader", note: "Node or OpenSSL runtime loader or startup control" },
  { value: "NODE_UNIQUE_ID", invariant: "process_loader", note: "Node or OpenSSL runtime loader or startup control" },
  { value: "NODE_PRESERVE_SYMLINKS", invariant: "process_loader", note: "Node or OpenSSL runtime loader or startup control" },
  { value: "NODE_PRESERVE_SYMLINKS_MAIN", invariant: "process_loader", note: "Node or OpenSSL runtime loader or startup control" },
  { value: "NODE_ICU_DATA", invariant: "process_loader", note: "Node or OpenSSL runtime loader or startup control" },
  { value: "NODE_COMPILE_CACHE", invariant: "process_loader", note: "Node or OpenSSL runtime loader or startup control" },
  { value: "NODE_COMPILE_CACHE_PORTABLE", invariant: "process_loader", note: "Node or OpenSSL runtime loader or startup control" },
  { value: "NODE_DISABLE_COMPILE_CACHE", invariant: "process_loader", note: "Node or OpenSSL runtime loader or startup control" },
  { value: "OPENSSL_CONF", invariant: "process_loader", note: "Node or OpenSSL runtime loader or startup control" },
  { value: "OPENSSL_CONF_INCLUDE", invariant: "process_loader", note: "Node or OpenSSL runtime loader or startup control" },
  { value: "OPENSSL_ENGINES", invariant: "process_loader", note: "Node or OpenSSL runtime loader or startup control" },
  { value: "OPENSSL_MODULES", invariant: "process_loader", note: "Node or OpenSSL runtime loader or startup control" },
  { value: "UV_THREADPOOL_SIZE", invariant: "process_loader", note: "Node or OpenSSL runtime loader or startup control" },
  { value: "NODE_DEBUG", invariant: "harness_control", note: "debug, telemetry or control switch read by the Node runtime or the Claude CLI" },
  { value: "NODE_DEBUG_NATIVE", invariant: "harness_control", note: "debug, telemetry or control switch read by the Node runtime or the Claude CLI" },
  { value: "NODE_V8_COVERAGE", invariant: "harness_control", note: "debug, telemetry or control switch read by the Node runtime or the Claude CLI" },
  { value: "NODE_REDIRECT_WARNINGS", invariant: "harness_control", note: "debug, telemetry or control switch read by the Node runtime or the Claude CLI" },
  { value: "NODE_PENDING_DEPRECATION", invariant: "harness_control", note: "debug, telemetry or control switch read by the Node runtime or the Claude CLI" },
  { value: "BETA_TRACING_ENDPOINT", invariant: "harness_control", note: "debug, telemetry or control switch read by the Node runtime or the Claude CLI" },
  { value: "SESSION_INGRESS_URL", invariant: "harness_control", note: "debug, telemetry or control switch read by the Node runtime or the Claude CLI" },
  { value: "SDK_NATIVE_BIN", invariant: "harness_control", note: "debug, telemetry or control switch read by the Node runtime or the Claude CLI" },
  { value: "AUTOMODE_DECISION_LOG", invariant: "harness_control", note: "debug, telemetry or control switch read by the Node runtime or the Claude CLI" },
  { value: "SRT_DEBUG", invariant: "harness_control", note: "debug, telemetry or control switch read by the Node runtime or the Claude CLI" },
  { value: "DEBUG_AUTH", invariant: "harness_control", note: "debug, telemetry or control switch read by the Node runtime or the Claude CLI" },
  { value: "API_FORCE_IDLE_TIMEOUT", invariant: "harness_control", note: "debug, telemetry or control switch read by the Node runtime or the Claude CLI" },
  { value: "SLASH_COMMAND_TOOL_CHAR_BUDGET", invariant: "harness_control", note: "debug, telemetry or control switch read by the Node runtime or the Claude CLI" },
  { value: "ENABLE_CLAUDEAI_MCP_SERVERS", invariant: "harness_control", note: "debug, telemetry or control switch read by the Node runtime or the Claude CLI" },
  { value: "ENABLE_TOOL_SEARCH", invariant: "harness_control", note: "debug, telemetry or control switch read by the Node runtime or the Claude CLI" },
  { value: "ENABLE_ENHANCED_TELEMETRY_BETA", invariant: "harness_control", note: "debug, telemetry or control switch read by the Node runtime or the Claude CLI" },
  { value: "IS_SANDBOX", invariant: "harness_control", note: "debug, telemetry or control switch read by the Node runtime or the Claude CLI" },
  { value: "COMPUTER_USE_SWIFT_NODE_PATH", invariant: "harness_control", note: "debug, telemetry or control switch read by the Node runtime or the Claude CLI" },
  { value: "DS_CHROMIUM_PATH", invariant: "harness_control", note: "debug, telemetry or control switch read by the Node runtime or the Claude CLI" },
  { value: "NODE_USE_ENV_PROXY", invariant: "network_wiring", note: "TLS or proxy behavior of the Node runtime" },
  { value: "NODE_USE_SYSTEM_CA", invariant: "network_wiring", note: "TLS or proxy behavior of the Node runtime" },
  { value: "SSLKEYLOGFILE", invariant: "network_wiring", note: "would log the TLS keys of the worker's provider traffic" },
  { value: "AGENT_PROXY_AUTH_TOKEN", invariant: "provider_auth", note: "credential of the CLI agent proxy" },
  { value: "AWS_BEARER_TOKEN_BEDROCK", invariant: "provider_auth", note: "Bedrock bearer credential the CLI reads without a provider selector" },
  { value: "CLOUDSDK_AUTH_ACCESS_TOKEN", invariant: "provider_auth", note: "Google access token the CLI reads without the Vertex selector" },
  { value: "ENVIRONMENT_SERVICE_KEY", invariant: "provider_auth", note: "CLI environment service credential" },
  { value: "USE_LOCAL_OAUTH", invariant: "provider_auth", note: "switches the CLI to a local OAuth endpoint" },
  { value: "USE_STAGING_OAUTH", invariant: "provider_auth", note: "switches the CLI to a staging OAuth endpoint" },
  { value: "AGENT_PROXY_URL", invariant: "network_wiring", note: "routes the CLI traffic through an agent proxy" },
  { value: "VOICE_STREAM_BASE_URL", invariant: "network_wiring", note: "endpoint override of a CLI network service" },
  { value: "CACERT_UPDATES", invariant: "network_wiring", note: "trust root handling of the CLI" },
  { value: "AWS_CA_BUNDLE", invariant: "network_wiring", note: "trust roots for the AWS SDKs" },
  { value: "CARGO_HTTP_CAINFO", invariant: "network_wiring", note: "trust roots for cargo" },
  { value: "DENO_CERT", invariant: "network_wiring", note: "trust roots for Deno" },
  { value: "GRPC_DEFAULT_SSL_ROOTS_FILE_PATH", invariant: "network_wiring", note: "trust roots for gRPC" },
  { value: "HEX_CACERTS_PATH", invariant: "network_wiring", note: "trust roots for Hex" },
  { value: "HTTPLIB2_CA_CERTS", invariant: "network_wiring", note: "trust roots for httplib2" },
  { value: "PIP_CERT", invariant: "network_wiring", note: "trust roots for pip" },
  { value: "CLOUDSDK_CORE_CUSTOM_CA_CERTS_FILE", invariant: "network_wiring", note: "trust roots for the gcloud CLI" },
  { value: "VITALS_EMITTER_BIN", invariant: "process_loader", note: "binary the CLI executes for its vitals emitter" },
  { value: "PATHEXT", invariant: "process_loader", note: "executable lookup extensions" },
  { value: "NODE_CLUSTER_SCHED_POLICY", invariant: "process_loader", note: "Node or OpenSSL runtime loader or startup control" },
  { value: "NODE_TEST_CONTEXT", invariant: "process_loader", note: "Node or OpenSSL runtime loader or startup control" },
  { value: "NODE_TEST_WORKER_ID", invariant: "process_loader", note: "Node or OpenSSL runtime loader or startup control" },
  { value: "CDPATH", invariant: "shell_semantics", note: "shell directory search path" },
  { value: "ZDOTDIR", invariant: "shell_semantics", note: "zsh startup file directory" },
  { value: "HISTFILE", invariant: "shell_semantics", note: "shell history file location" },
  { value: "COMSPEC", invariant: "shell_semantics", note: "command interpreter selection" },
  { value: "TMP", invariant: "managed_dirs", note: "temporary directory the worker manages" },
  { value: "TEMP", invariant: "managed_dirs", note: "temporary directory the worker manages" },
  { value: "PWD", invariant: "managed_dirs", note: "working directory the worker manages" },
  { value: "ALLUSERSPROFILE", invariant: "managed_dirs", note: "profile directory the worker manages" },
  { value: "APPDATA", invariant: "managed_dirs", note: "profile directory the worker manages" },
  { value: "HOMEDRIVE", invariant: "managed_dirs", note: "home location the worker manages" },
  { value: "HOMEPATH", invariant: "managed_dirs", note: "home location the worker manages" },
  { value: "HOMESHARE", invariant: "managed_dirs", note: "home location the worker manages" },
  { value: "LOCALAPPDATA", invariant: "managed_dirs", note: "profile directory the worker manages" },
  { value: "PROGRAMDATA", invariant: "managed_dirs", note: "profile directory the worker manages" },
  { value: "SYSTEMROOT", invariant: "managed_dirs", note: "system directory selection" },
  { value: "USERPROFILE", invariant: "managed_dirs", note: "profile directory the worker manages" },
  { value: "HUB_CONFIG", invariant: "git_authority", note: "hub CLI configuration and credentials" },
  { value: "DEBUG", invariant: "harness_control", note: "debug, telemetry or control switch read by the Node runtime or the Claude CLI" },
  { value: "DEBUG_SDK", invariant: "harness_control", note: "debug, telemetry or control switch read by the Node runtime or the Claude CLI" },
  { value: "DEBUG_CLAUDE_AGENT_SDK", invariant: "harness_control", note: "debug, telemetry or control switch read by the Node runtime or the Claude CLI" },
  { value: "FORCE_VCR", invariant: "harness_control", note: "debug, telemetry or control switch read by the Node runtime or the Claude CLI" },
  { value: "VCR_RECORD", invariant: "harness_control", note: "debug, telemetry or control switch read by the Node runtime or the Claude CLI" },
  { value: "ULTRAPLAN_PROMPT_FILE", invariant: "harness_control", note: "debug, telemetry or control switch read by the Node runtime or the Claude CLI" },
  { value: "SYSTEM_REMINDER_MEMORY_CONTEXT", invariant: "harness_control", note: "debug, telemetry or control switch read by the Node runtime or the Claude CLI" },
  { value: "USE_BUILTIN_RIPGREP", invariant: "harness_control", note: "debug, telemetry or control switch read by the Node runtime or the Claude CLI" },
  { value: "EMBEDDED_SEARCH_TOOLS", invariant: "harness_control", note: "debug, telemetry or control switch read by the Node runtime or the Claude CLI" },
  { value: "USE_API_CONTEXT_MANAGEMENT", invariant: "harness_control", note: "debug, telemetry or control switch read by the Node runtime or the Claude CLI" },
  { value: "FALLBACK_FOR_ALL_PRIMARY_MODELS", invariant: "harness_control", note: "debug, telemetry or control switch read by the Node runtime or the Claude CLI" },
  { value: "MAX_STRUCTURED_OUTPUT_RETRIES", invariant: "harness_control", note: "debug, telemetry or control switch read by the Node runtime or the Claude CLI" },
  { value: "FORCE_PROMPT_CACHING_5M", invariant: "harness_control", note: "debug, telemetry or control switch read by the Node runtime or the Claude CLI" },
  { value: "ENABLE_PROMPT_CACHING_1H", invariant: "harness_control", note: "debug, telemetry or control switch read by the Node runtime or the Claude CLI" },
  { value: "ENABLE_PROMPT_CACHING_1H_BEDROCK", invariant: "harness_control", note: "debug, telemetry or control switch read by the Node runtime or the Claude CLI" },
  { value: "ENABLE_SESSION_PERSISTENCE", invariant: "harness_control", note: "debug, telemetry or control switch read by the Node runtime or the Claude CLI" },
  { value: "ENABLE_LSP_TOOL", invariant: "harness_control", note: "debug, telemetry or control switch read by the Node runtime or the Claude CLI" },
  { value: "ENABLE_MCP_LARGE_OUTPUT_FILES", invariant: "harness_control", note: "debug, telemetry or control switch read by the Node runtime or the Claude CLI" },
  { value: "ENABLE_BETA_TRACING_DETAILED", invariant: "harness_control", note: "debug, telemetry or control switch read by the Node runtime or the Claude CLI" },
  { value: "LOCAL_BRIDGE", invariant: "harness_control", note: "debug, telemetry or control switch read by the Node runtime or the Claude CLI" },
  { value: "ENABLE_LOCKLESS_UPDATES", invariant: "harness_control", note: "debug, telemetry or control switch read by the Node runtime or the Claude CLI" },
  { value: "ENABLE_PID_BASED_VERSION_LOCKING", invariant: "harness_control", note: "debug, telemetry or control switch read by the Node runtime or the Claude CLI" },
  { value: "ENABLE_SESSION_BACKGROUNDING", invariant: "harness_control", note: "debug, telemetry or control switch read by the Node runtime or the Claude CLI" },
  { value: "FORCE_AUTOUPDATE_PLUGINS", invariant: "harness_control", note: "debug, telemetry or control switch read by the Node runtime or the Claude CLI" },
  { value: "FORCE_CODE_TERMINAL", invariant: "harness_control", note: "debug, telemetry or control switch read by the Node runtime or the Claude CLI" },
  { value: "INK_SCREEN_READER", invariant: "harness_control", note: "debug, telemetry or control switch read by the Node runtime or the Claude CLI" },
  { value: "TEST_ENABLE_SESSION_PERSISTENCE", invariant: "harness_control", note: "debug, telemetry or control switch read by the Node runtime or the Claude CLI" },
  { value: "TEST_GRACEFUL_FS_GLOBAL_PATCH", invariant: "harness_control", note: "debug, telemetry or control switch read by the Node runtime or the Claude CLI" },
  { value: "ALLOW_ANT_COMPUTER_USE_MCP", invariant: "harness_control", note: "debug, telemetry or control switch read by the Node runtime or the Claude CLI" },
  { value: "CLAUBBIT", invariant: "harness_control", note: "debug, telemetry or control switch read by the Node runtime or the Claude CLI" },
  { value: "CLAUDECODE", invariant: "harness_control", note: "debug, telemetry or control switch read by the Node runtime or the Claude CLI" },
  { value: "AI_AGENT", invariant: "harness_control", note: "debug, telemetry or control switch read by the Node runtime or the Claude CLI" },
  { value: "IS_DEMO", invariant: "harness_control", note: "debug, telemetry or control switch read by the Node runtime or the Claude CLI" },
  { value: "DEMO_VERSION", invariant: "harness_control", note: "debug, telemetry or control switch read by the Node runtime or the Claude CLI" },
  { value: "SAFEUSER", invariant: "harness_control", note: "debug, telemetry or control switch read by the Node runtime or the Claude CLI" },
  { value: "VERBOSE_SSR", invariant: "harness_control", note: "debug, telemetry or control switch read by the Node runtime or the Claude CLI" },
  { value: "SWE_BENCH_INSTANCE_ID", invariant: "harness_control", note: "debug, telemetry or control switch read by the Node runtime or the Claude CLI" },
  { value: "SWE_BENCH_RUN_ID", invariant: "harness_control", note: "debug, telemetry or control switch read by the Node runtime or the Claude CLI" },
  { value: "SWE_BENCH_TASK_ID", invariant: "harness_control", note: "debug, telemetry or control switch read by the Node runtime or the Claude CLI" },
  { value: "NODE", invariant: "process_loader", note: "node executable path read by npm scripts and tools that spawn Node" },
  { value: "NODE_NO_WARNINGS", invariant: "harness_control", note: "suppresses Node process warnings, harness included" },
  { value: "NODE_SKIP_PLATFORM_CHECK", invariant: "process_loader", note: "skips the Node platform check at startup" },
  { value: "NODE_PENDING_PIPE_INSTANCES", invariant: "process_loader", note: "Node pipe instance count on Windows" },
  { value: "UV_USE_IO_URING", invariant: "process_loader", note: "libuv I/O backend of every Node process, harness included" },
  { value: "AWS_EC2_METADATA_SERVICE_ENDPOINT", invariant: "network_wiring", note: "metadata endpoint the harness's AWS SDK reads" },
  { value: "AWS_ENDPOINT_URL", invariant: "network_wiring", note: "global AWS endpoint override; it also routes the Bedrock traffic the AWS_ENDPOINT_URL_BEDROCK prefix is denied for" },
  { value: "AWS_ENDPOINT_URL_STS", invariant: "network_wiring", note: "AWS STS endpoint override; it redirects credential exchange" },
  { value: "AWS_ENDPOINT_URL_SSO", invariant: "network_wiring", note: "AWS SSO endpoint override; it redirects credential acquisition" },
  { value: "AWS_ENDPOINT_URL_SSO_OIDC", invariant: "network_wiring", note: "AWS SSO OIDC endpoint override; it redirects credential acquisition" },
  { value: "NODE_INSPECTOR_IPC", invariant: "harness_control", note: "TypeScript debug-mode selector read by the bundled Claude CLI" },
  { value: "VSCODE_INSPECTOR_OPTIONS", invariant: "harness_control", note: "TypeScript debug-mode selector read by the bundled Claude CLI" },
  { value: "SYSTEMDRIVE", invariant: "managed_dirs", note: "system directory selection (uppercase spelling of the Windows SystemDrive)" },
  { value: "_CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL", invariant: "provider_auth", note: "makes the Claude CLI treat the worker-set gateway as the first-party Anthropic API" },
  { value: "SPIFFE_ENDPOINT_SOCKET", invariant: "provider_auth", note: "workload identity endpoint Codex reads to obtain provider credentials" },
  { value: "SSH2_CLIENT", invariant: "git_authority", note: "its presence changes Codex network proxy credential brokerage" },
  { value: "OTEL_RESOURCE_ATTRIBUTES", invariant: "harness_control", note: "telemetry resource attributes and statusline collector eligibility in Codex" },
];

/** Name prefixes reserved for the worker and the agent harnesses. */
export const RUN_ENV_DENIED_PREFIXES: readonly RunEnvDeniedRule[] = [
  { value: "UZI_", invariant: "worker_credentials", note: "worker and run control plane" },
  { value: "CLAUDE_", invariant: "harness_control", note: "Claude CLI settings, including provider selectors" },
  { value: "ANTHROPIC_", invariant: "provider_auth", note: "Anthropic provider configuration and credentials" },
  { value: "CODEX_", invariant: "provider_auth", note: "Codex runtime settings and credentials" },
  { value: "OPENAI_", invariant: "provider_auth", note: "OpenAI provider configuration and credentials" },
  { value: "MCP_", invariant: "harness_control", note: "MCP tool settings of the harness" },
  { value: "DISABLE_", invariant: "harness_control", note: "feature switches read by the Claude CLI" },
  { value: "GH_", invariant: "git_authority", note: "GitHub CLI authority" },
  { value: "GITHUB_", invariant: "git_authority", note: "GitHub forge authority" },
  { value: "GITLAB_", invariant: "git_authority", note: "GitLab forge authority" },
  { value: "GLAB_", invariant: "git_authority", note: "GitLab CLI authority" },
  { value: "GIT_", invariant: "git_authority", note: "git behavior and credentials" },
  { value: "SSH_", invariant: "git_authority", note: "ssh agent and transport authority" },
  { value: "LD_", invariant: "process_loader", note: "dynamic loader injection" },
  { value: "DYLD_", invariant: "process_loader", note: "dynamic loader injection" },
  { value: "BASH_", invariant: "shell_semantics", note: "bash startup and function import" },
  { value: "XDG_", invariant: "managed_dirs", note: "XDG base directories the worker manages" },
  { value: "SSL_CERT_", invariant: "network_wiring", note: "OpenSSL trust roots" },
  { value: "PYTHON", invariant: "process_loader", note: "Python interpreter startup and path" },
  { value: "PERL5", invariant: "process_loader", note: "Perl module path and options" },
  { value: "NIX_", invariant: "process_loader", note: "Nix toolchain wiring" },
  { value: "DEVBOX_", invariant: "process_loader", note: "Devbox toolchain wiring" },
  { value: "BUN_", invariant: "harness_control", note: "Bun runtime controls (BUN_CONFIG_VERBOSE_FETCH prints request headers, BUN_BE_BUN, BUN_OPTIONS, BUN_INSPECT)" },
  { value: "CCR_", invariant: "harness_control", note: "Claude CLI internal controls (CCR_AGENT_PROXY_TOKEN_FILE_DESCRIPTOR is auth)" },
  { value: "SELF_HOSTED_RUNNER_", invariant: "harness_control", note: "Claude CLI internal controls" },
  { value: "BUGHUNTER_", invariant: "harness_control", note: "Claude CLI internal controls" },
  { value: "_CLAUDE_", invariant: "harness_control", note: "Claude CLI internal settings behind a leading underscore" },
  { value: "ANT_OTEL_", invariant: "harness_control", note: "Claude CLI internal telemetry exporter settings (ANT_OTEL_*); narrower than ANT_ so Apache Ant's ANT_OPTS and ANT_HOME stay allowed" },
  { value: "ANT_CLAUDE_", invariant: "harness_control", note: "Claude CLI internal metrics endpoint (ANT_CLAUDE_CODE_METRICS_ENDPOINT)" },
  { value: "OTEL_LOG_", invariant: "harness_control", note: "Claude CLI telemetry content switches (OTEL_LOG_RAW_API_BODIES, OTEL_LOG_USER_PROMPTS, OTEL_LOG_TOOL_CONTENT)" },
  { value: "LC_", invariant: "shell_semantics", note: "locale of the commands the worker runs (LC_ALL, LC_CTYPE), like LANG" },
  { value: "GCM_", invariant: "git_authority", note: "Git Credential Manager configuration" },
  { value: "OTEL_EXPORTER_OTLP_", invariant: "network_wiring", note: "telemetry exporter endpoints and headers the harness reads" },
  { value: "GCE_METADATA_", invariant: "network_wiring", note: "metadata server address the harness's Google auth reads" },
  { value: "AWS_ENDPOINT_URL_BEDROCK", invariant: "network_wiring", note: "Bedrock endpoint the harness reads; covers AWS_ENDPOINT_URL_BEDROCK_RUNTIME" },
  { value: "AWS_ENDPOINT_URL_", invariant: "network_wiring", note: "AWS per-service endpoint overrides (Codex 0.171.0 reserves the whole family); also covers the more specific AWS_ENDPOINT_URL_BEDROCK prefix and the STS and SSO names above, which stay for their documented rationale" },
];

/** Name suffixes that change network routing. */
export const RUN_ENV_DENIED_SUFFIXES: readonly RunEnvDeniedRule[] = [
  { value: "_PROXY", invariant: "network_wiring", note: "names ending in it change network routing" },
];

/** The shape of a variable name. */
const RUN_ENV_NAME_REGEXP = /^[A-Z_][A-Z0-9_]{0,127}$/;

/** Returns the rule a name breaks, or null when it is allowed. Same order and
 *  semantics as ValidateName in api/internal/runenv/names.go: shape, then exact
 *  names, then prefixes, then suffixes. */
export function runEnvNameViolation(name: string): string | null {
  if (!RUN_ENV_NAME_REGEXP.test(name)) {
    return "must match ^[A-Z_][A-Z0-9_]{0,127}$ (upper-case letters, digits and underscores, not starting with a digit, at most 128 characters)";
  }
  if (RUN_ENV_DENIED_NAMES.some((r) => r.value === name)) {
    return `${name} is set or reserved by the worker (denied name)`;
  }
  for (const { value: p } of RUN_ENV_DENIED_PREFIXES) {
    if (name.startsWith(p)) {
      return `denied prefix ${p}: names starting with it are reserved for the worker and the agent harness`;
    }
  }
  for (const { value: s } of RUN_ENV_DENIED_SUFFIXES) {
    if (name.endsWith(s)) {
      return `denied suffix ${s}: names ending in it change network routing and are reserved`;
    }
  }
  return null;
}
