package runenv

import (
	"fmt"
	"regexp"
	"strings"
)

// The three lists below are the Go copy of fixtures/run-env/name-rules.json, which
// also records the harness versions they were derived from and which the worker
// pins as well. names_test.go fails when the copy and the fixture differ.

// deniedRule is one denylist entry: the Value that is matched, the worker invariant it
// protects, and a short note saying why. Only Value takes part in matching.
type deniedRule struct {
	Value     string
	Invariant string
	Note      string
}

// deniedNames are exact names a variable may not use: the worker's own invariants
// and guardrails (its credentials, Docker endpoint, git authority, provider
// configuration and managed directories) plus the harness controls outside the
// denied prefixes.
var deniedNames = []deniedRule{
	{"HOME", "managed_dirs", "home directory the worker manages for the run"},
	{"PATH", "process_loader", "command lookup path the worker controls"},
	{"TMPDIR", "managed_dirs", "temporary directory the worker manages"},
	{"LANG", "shell_semantics", "locale of the commands the worker runs"},
	{"SHELL", "shell_semantics", "shell the agent harness spawns"},
	{"ENV", "shell_semantics", "POSIX shell startup file"},
	{"IFS", "shell_semantics", "shell field separator"},
	{"PS4", "shell_semantics", "shell trace prompt, expanded by the shell"},
	{"PROMPT_COMMAND", "shell_semantics", "bash command run before each prompt"},
	{"SHELLOPTS", "shell_semantics", "bash options applied to every shell"},
	{"BASHOPTS", "shell_semantics", "bash options applied to every shell"},
	{"DOCKER_HOST", "network_wiring", "Docker endpoint the worker provisions"},
	{"DOCKER_CONFIG", "network_wiring", "Docker client config and credentials the worker provisions"},
	{"DOCKER_CONTEXT", "network_wiring", "Docker endpoint selection"},
	{"DOCKER_TLS_VERIFY", "network_wiring", "Docker endpoint TLS wiring"},
	{"DOCKER_CERT_PATH", "network_wiring", "Docker endpoint TLS wiring"},
	{"BUILDKIT_HOST", "network_wiring", "BuildKit endpoint the worker provisions"},
	{"GOMAXPROCS", "harness_control", "CPU budget the worker sets for Go processes"},
	{"GOFLAGS", "harness_control", "Go build flags the worker sets"},
	{"GOTMPDIR", "managed_dirs", "Go temporary directory the worker manages"},
	{"GOMODCACHE", "managed_dirs", "Go module cache the worker manages"},
	{"GOCACHE", "managed_dirs", "Go build cache the worker manages"},
	{"GOPROXY", "network_wiring", "Go module download routing"},
	{"GOSUMDB", "network_wiring", "Go checksum database routing"},
	{"GOENV", "managed_dirs", "Go environment file location"},
	{"GOTOOLCHAIN", "process_loader", "Go toolchain selection and download"},
	{"NPM_CONFIG_CACHE", "managed_dirs", "npm cache directory the worker manages"},
	{"npm_config_cache", "managed_dirs", "npm cache directory the worker manages"},
	{"NODE_OPTIONS", "process_loader", "Node runtime flags applied to every Node process, harness included"},
	{"NODE_PATH", "process_loader", "Node module resolution path"},
	{"NODE_EXTRA_CA_CERTS", "network_wiring", "extra trust roots for every Node process"},
	{"NODE_TLS_REJECT_UNAUTHORIZED", "network_wiring", "disables TLS verification for every Node process"},
	{"JAVA_TOOL_OPTIONS", "process_loader", "JVM flags applied to every JVM"},
	{"_JAVA_OPTIONS", "process_loader", "JVM flags applied to every JVM"},
	{"JDK_JAVA_OPTIONS", "process_loader", "JVM flags applied by the java launcher"},
	{"CURL_HOME", "managed_dirs", "curl config directory"},
	{"NETRC", "git_authority", "netrc file carries forge credentials"},
	{"WGETRC", "network_wiring", "wget config can set proxies and certificates"},
	{"CURL_CA_BUNDLE", "network_wiring", "trust roots for curl"},
	{"REQUESTS_CA_BUNDLE", "network_wiring", "trust roots for Python requests"},
	{"RUBYOPT", "process_loader", "Ruby flags applied to every Ruby process"},
	{"AGENT_BROWSER_ARGS", "harness_control", "browser launch arguments of the agent browser tool"},
	{"AGENT_BROWSER_EXECUTABLE_PATH", "harness_control", "browser binary of the agent browser tool"},
	{"CHROME_PATH", "harness_control", "browser binary used by the agent browser tool"},
	{"CLAUDE_CODE_OAUTH_TOKEN", "worker_credentials", "Anthropic subscription token the worker injects"},
	{"ANTHROPIC_API_KEY", "provider_auth", "Anthropic API credential"},
	{"ANTHROPIC_AUTH_TOKEN", "provider_auth", "Anthropic gateway credential"},
	{"UZI_SQLC_DIR", "harness_control", "worker-managed sqlc location"},
	{"UZI_RUN_ATTEMPT", "harness_control", "worker-managed run attempt identity"},
	{"UZI_RUN_CLONE", "harness_control", "worker-managed run clone path"},
	{"UZI_RUN_CLONE_KEY", "harness_control", "worker-managed run clone identity"},
	{"UZI_WORKER_SPAWN", "harness_control", "worker-managed spawn marker"},
	{"GIT_CONFIG_COUNT", "git_authority", "git config injection"},
	{"GIT_CONFIG_PARAMETERS", "git_authority", "git config injection"},
	{"API_TIMEOUT_MS", "harness_control", "request timeout of the Claude CLI"},
	{"MAX_THINKING_TOKENS", "harness_control", "thinking budget of the Claude CLI"},
	{"MAX_MCP_OUTPUT_TOKENS", "harness_control", "tool output bound of the Claude CLI"},
	{"NODE_REPL_EXTERNAL_MODULE", "process_loader", "Node or OpenSSL runtime loader or startup control"},
	{"NODE_CHANNEL_FD", "process_loader", "Node or OpenSSL runtime loader or startup control"},
	{"NODE_CHANNEL_SERIALIZATION_MODE", "process_loader", "Node or OpenSSL runtime loader or startup control"},
	{"NODE_UNIQUE_ID", "process_loader", "Node or OpenSSL runtime loader or startup control"},
	{"NODE_PRESERVE_SYMLINKS", "process_loader", "Node or OpenSSL runtime loader or startup control"},
	{"NODE_PRESERVE_SYMLINKS_MAIN", "process_loader", "Node or OpenSSL runtime loader or startup control"},
	{"NODE_ICU_DATA", "process_loader", "Node or OpenSSL runtime loader or startup control"},
	{"NODE_COMPILE_CACHE", "process_loader", "Node or OpenSSL runtime loader or startup control"},
	{"NODE_COMPILE_CACHE_PORTABLE", "process_loader", "Node or OpenSSL runtime loader or startup control"},
	{"NODE_DISABLE_COMPILE_CACHE", "process_loader", "Node or OpenSSL runtime loader or startup control"},
	{"OPENSSL_CONF", "process_loader", "Node or OpenSSL runtime loader or startup control"},
	{"OPENSSL_CONF_INCLUDE", "process_loader", "Node or OpenSSL runtime loader or startup control"},
	{"OPENSSL_ENGINES", "process_loader", "Node or OpenSSL runtime loader or startup control"},
	{"OPENSSL_MODULES", "process_loader", "Node or OpenSSL runtime loader or startup control"},
	{"UV_THREADPOOL_SIZE", "process_loader", "Node or OpenSSL runtime loader or startup control"},
	{"NODE_DEBUG", "harness_control", "debug, telemetry or control switch read by the Node runtime or the Claude CLI"},
	{"NODE_DEBUG_NATIVE", "harness_control", "debug, telemetry or control switch read by the Node runtime or the Claude CLI"},
	{"NODE_V8_COVERAGE", "harness_control", "debug, telemetry or control switch read by the Node runtime or the Claude CLI"},
	{"NODE_REDIRECT_WARNINGS", "harness_control", "debug, telemetry or control switch read by the Node runtime or the Claude CLI"},
	{"NODE_PENDING_DEPRECATION", "harness_control", "debug, telemetry or control switch read by the Node runtime or the Claude CLI"},
	{"OTEL_LOG_RAW_API_BODIES", "harness_control", "debug, telemetry or control switch read by the Node runtime or the Claude CLI"},
	{"OTEL_LOG_TOOL_CONTENT", "harness_control", "debug, telemetry or control switch read by the Node runtime or the Claude CLI"},
	{"BETA_TRACING_ENDPOINT", "harness_control", "debug, telemetry or control switch read by the Node runtime or the Claude CLI"},
	{"SESSION_INGRESS_URL", "harness_control", "debug, telemetry or control switch read by the Node runtime or the Claude CLI"},
	{"SDK_NATIVE_BIN", "harness_control", "debug, telemetry or control switch read by the Node runtime or the Claude CLI"},
	{"AUTOMODE_DECISION_LOG", "harness_control", "debug, telemetry or control switch read by the Node runtime or the Claude CLI"},
	{"SRT_DEBUG", "harness_control", "debug, telemetry or control switch read by the Node runtime or the Claude CLI"},
	{"DEBUG_AUTH", "harness_control", "debug, telemetry or control switch read by the Node runtime or the Claude CLI"},
	{"API_FORCE_IDLE_TIMEOUT", "harness_control", "debug, telemetry or control switch read by the Node runtime or the Claude CLI"},
	{"SLASH_COMMAND_TOOL_CHAR_BUDGET", "harness_control", "debug, telemetry or control switch read by the Node runtime or the Claude CLI"},
	{"ENABLE_CLAUDEAI_MCP_SERVERS", "harness_control", "debug, telemetry or control switch read by the Node runtime or the Claude CLI"},
	{"ENABLE_TOOL_SEARCH", "harness_control", "debug, telemetry or control switch read by the Node runtime or the Claude CLI"},
	{"ENABLE_ENHANCED_TELEMETRY_BETA", "harness_control", "debug, telemetry or control switch read by the Node runtime or the Claude CLI"},
	{"IS_SANDBOX", "harness_control", "debug, telemetry or control switch read by the Node runtime or the Claude CLI"},
	{"COMPUTER_USE_SWIFT_NODE_PATH", "harness_control", "debug, telemetry or control switch read by the Node runtime or the Claude CLI"},
	{"DS_CHROMIUM_PATH", "harness_control", "debug, telemetry or control switch read by the Node runtime or the Claude CLI"},
	{"NODE_USE_ENV_PROXY", "network_wiring", "TLS or proxy behavior of the Node runtime"},
	{"NODE_USE_SYSTEM_CA", "network_wiring", "TLS or proxy behavior of the Node runtime"},
	{"SSLKEYLOGFILE", "network_wiring", "would log the TLS keys of the worker's provider traffic"},
}

// deniedPrefixes are name prefixes reserved for the worker and the agent harnesses.
var deniedPrefixes = []deniedRule{
	{"UZI_", "worker_credentials", "worker and run control plane"},
	{"CLAUDE_", "harness_control", "Claude CLI settings, including provider selectors"},
	{"ANTHROPIC_", "provider_auth", "Anthropic provider configuration and credentials"},
	{"CODEX_", "provider_auth", "Codex runtime settings and credentials"},
	{"OPENAI_", "provider_auth", "OpenAI provider configuration and credentials"},
	{"MCP_", "harness_control", "MCP tool settings of the harness"},
	{"DISABLE_", "harness_control", "feature switches read by the Claude CLI"},
	{"GH_", "git_authority", "GitHub CLI authority"},
	{"GITHUB_", "git_authority", "GitHub forge authority"},
	{"GITLAB_", "git_authority", "GitLab forge authority"},
	{"GLAB_", "git_authority", "GitLab CLI authority"},
	{"GIT_", "git_authority", "git behavior and credentials"},
	{"SSH_", "git_authority", "ssh agent and transport authority"},
	{"LD_", "process_loader", "dynamic loader injection"},
	{"DYLD_", "process_loader", "dynamic loader injection"},
	{"BASH_", "shell_semantics", "bash startup and function import"},
	{"XDG_", "managed_dirs", "XDG base directories the worker manages"},
	{"SSL_CERT_", "network_wiring", "OpenSSL trust roots"},
	{"PYTHON", "process_loader", "Python interpreter startup and path"},
	{"PERL5", "process_loader", "Perl module path and options"},
	{"NIX_", "process_loader", "Nix toolchain wiring"},
	{"DEVBOX_", "process_loader", "Devbox toolchain wiring"},
	{"BUN_", "harness_control", "Bun runtime controls (BUN_CONFIG_VERBOSE_FETCH prints request headers, BUN_BE_BUN, BUN_OPTIONS, BUN_INSPECT)"},
	{"CCR_", "harness_control", "Claude CLI internal controls (CCR_AGENT_PROXY_TOKEN_FILE_DESCRIPTOR is auth)"},
	{"SELF_HOSTED_RUNNER_", "harness_control", "Claude CLI internal controls"},
	{"BUGHUNTER_", "harness_control", "Claude CLI internal controls"},
}

// deniedSuffixes are name suffixes that change network routing.
var deniedSuffixes = []deniedRule{
	{"_PROXY", "network_wiring", "names ending in it change network routing"},
}

// NameRegexp is the shape of a variable name.
var NameRegexp = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,127}$`)

// MaxNameBytes is the longest name NameRegexp admits.
const MaxNameBytes = 128

// RuleError names the rule a field broke. The message never carries a value.
type RuleError struct {
	Field string
	Rule  string
}

func (e *RuleError) Error() string { return e.Field + ": " + e.Rule }

// deniedBy returns the rule that denies name, or "" when no list does.
func deniedBy(name string) string {
	for _, r := range deniedNames {
		if name == r.Value {
			return fmt.Sprintf("%s is set or reserved by the worker (denied name)", name)
		}
	}
	for _, r := range deniedPrefixes {
		if strings.HasPrefix(name, r.Value) {
			return fmt.Sprintf("denied prefix %s: names starting with it are reserved for the worker and the agent harness", r.Value)
		}
	}
	for _, r := range deniedSuffixes {
		if strings.HasSuffix(name, r.Value) {
			return fmt.Sprintf("denied suffix %s: names ending in it change network routing and are reserved", r.Value)
		}
	}
	return ""
}

// ValidateName applies the name shape and the denylist. A non-nil result is a
// *RuleError whose message names the broken rule.
func ValidateName(name string) error {
	if !NameRegexp.MatchString(name) {
		return &RuleError{Field: "name", Rule: "must match ^[A-Z_][A-Z0-9_]{0,127}$ (upper-case letters, digits and underscores, not starting with a digit, at most 128 characters)"}
	}
	if rule := deniedBy(name); rule != "" {
		return &RuleError{Field: "name", Rule: rule}
	}
	return nil
}
