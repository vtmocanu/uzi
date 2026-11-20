package runenv

import (
	"fmt"
	"regexp"
	"strings"
)

// The three tables below are the Go copy of fixtures/run-env/name-rules.json, which
// also records the harness versions they were derived from and which the worker
// pins as well. names_test.go fails when the copy and the fixture differ.

// deniedRule is one entry of the shared fixture: the denied value, the worker
// invariant it protects and a short note on why.
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
	{"HOME", "managed_dirs", "directory the worker sets for the run home or temp space"},
	{"PATH", "process_loader", "search path the worker controls for every command"},
	{"TMPDIR", "managed_dirs", "directory the worker sets for the run home or temp space"},
	{"LANG", "shell_semantics", "locale and shell start-up behaviour the worker fixes"},
	{"SHELL", "shell_semantics", "locale and shell start-up behaviour the worker fixes"},
	{"ENV", "shell_semantics", "locale and shell start-up behaviour the worker fixes"},
	{"IFS", "shell_semantics", "locale and shell start-up behaviour the worker fixes"},
	{"PS4", "shell_semantics", "locale and shell start-up behaviour the worker fixes"},
	{"PROMPT_COMMAND", "shell_semantics", "locale and shell start-up behaviour the worker fixes"},
	{"SHELLOPTS", "shell_semantics", "locale and shell start-up behaviour the worker fixes"},
	{"BASHOPTS", "shell_semantics", "locale and shell start-up behaviour the worker fixes"},
	{"DOCKER_HOST", "network_wiring", "the worker reaches its Docker daemon through these"},
	{"DOCKER_CONFIG", "managed_dirs", "the worker owns the Docker client configuration directory"},
	{"DOCKER_CONTEXT", "network_wiring", "the worker reaches its Docker daemon through these"},
	{"DOCKER_TLS_VERIFY", "network_wiring", "the worker reaches its Docker daemon through these"},
	{"DOCKER_CERT_PATH", "network_wiring", "the worker reaches its Docker daemon through these"},
	{"BUILDKIT_HOST", "network_wiring", "the worker reaches its Docker daemon through these"},
	{"GOMAXPROCS", "process_loader", "Go toolchain selection and resource limits the worker fixes"},
	{"GOFLAGS", "process_loader", "Go toolchain selection and resource limits the worker fixes"},
	{"GOTMPDIR", "managed_dirs", "Go build and module directories the worker places"},
	{"GOMODCACHE", "managed_dirs", "Go build and module directories the worker places"},
	{"GOCACHE", "managed_dirs", "Go build and module directories the worker places"},
	{"GOPROXY", "network_wiring", "Go module proxy and checksum database routing"},
	{"GOSUMDB", "network_wiring", "Go module proxy and checksum database routing"},
	{"GOENV", "process_loader", "Go toolchain selection and resource limits the worker fixes"},
	{"GOTOOLCHAIN", "process_loader", "Go toolchain selection and resource limits the worker fixes"},
	{"NPM_CONFIG_CACHE", "managed_dirs", "directory the worker sets for the run home or temp space"},
	{"npm_config_cache", "managed_dirs", "directory the worker sets for the run home or temp space"},
	{"NODE_OPTIONS", "process_loader", "Node start-up options and module search path"},
	{"NODE_PATH", "process_loader", "Node start-up options and module search path"},
	{"NODE_EXTRA_CA_CERTS", "network_wiring", "Node trust store and TLS verification"},
	{"NODE_TLS_REJECT_UNAUTHORIZED", "network_wiring", "Node trust store and TLS verification"},
	{"JAVA_TOOL_OPTIONS", "process_loader", "JVM and Ruby start-up options injected into every process"},
	{"_JAVA_OPTIONS", "process_loader", "JVM and Ruby start-up options injected into every process"},
	{"JDK_JAVA_OPTIONS", "process_loader", "JVM and Ruby start-up options injected into every process"},
	{"CURL_HOME", "network_wiring", "client configuration and credential files for curl and wget"},
	{"NETRC", "network_wiring", "client configuration and credential files for curl and wget"},
	{"WGETRC", "network_wiring", "client configuration and credential files for curl and wget"},
	{"CURL_CA_BUNDLE", "network_wiring", "trust store of curl and Python requests"},
	{"REQUESTS_CA_BUNDLE", "network_wiring", "trust store of curl and Python requests"},
	{"RUBYOPT", "process_loader", "JVM and Ruby start-up options injected into every process"},
	{"AGENT_BROWSER_ARGS", "process_loader", "browser discovery for the agent browser tool"},
	{"AGENT_BROWSER_EXECUTABLE_PATH", "process_loader", "browser discovery for the agent browser tool"},
	{"CHROME_PATH", "process_loader", "browser discovery for the agent browser tool"},
	{"CLAUDE_CODE_OAUTH_TOKEN", "provider_auth", "credential the worker selects for the model provider"},
	{"ANTHROPIC_API_KEY", "provider_auth", "credential the worker selects for the model provider"},
	{"ANTHROPIC_AUTH_TOKEN", "provider_auth", "credential the worker selects for the model provider"},
	{"UZI_SQLC_DIR", "managed_dirs", "run-attempt and clone markers the worker sets"},
	{"UZI_RUN_ATTEMPT", "managed_dirs", "run-attempt and clone markers the worker sets"},
	{"UZI_RUN_CLONE", "managed_dirs", "run-attempt and clone markers the worker sets"},
	{"UZI_RUN_CLONE_KEY", "managed_dirs", "run-attempt and clone markers the worker sets"},
	{"UZI_WORKER_SPAWN", "worker_credentials", "marker of a worker-spawned process"},
	{"GIT_CONFIG_COUNT", "git_authority", "git configuration injected through the environment"},
	{"GIT_CONFIG_PARAMETERS", "git_authority", "git configuration injected through the environment"},
	{"API_TIMEOUT_MS", "harness_control", "Claude CLI request timeout"},
	{"MAX_THINKING_TOKENS", "harness_control", "Claude CLI thinking budget"},
	{"MAX_MCP_OUTPUT_TOKENS", "harness_control", "Claude CLI tool output bound"},
	{"API_FORCE_IDLE_TIMEOUT", "harness_control", "Claude CLI idle-timeout override read by the pinned binary"},
	{"ENABLE_TOOL_SEARCH", "harness_control", "Claude CLI feature switch (tool search) read by the pinned binary"},
	{"ENABLE_ENHANCED_TELEMETRY_BETA", "harness_control", "Claude CLI telemetry switch read by the pinned binary"},
	{"ENABLE_CLAUDEAI_MCP_SERVERS", "harness_control", "Claude CLI switch for claude.ai MCP servers read by the pinned binary"},
	{"SLASH_COMMAND_TOOL_CHAR_BUDGET", "harness_control", "Claude CLI slash-command budget read by the pinned binary"},
	{"FORCE_CODE_TERMINAL", "harness_control", "Claude CLI terminal mode read by the pinned binary"},
	{"IS_SANDBOX", "harness_control", "Claude CLI sandbox-mode assertion read by the pinned binary"},
	{"SESSION_INGRESS_URL", "network_wiring", "Claude CLI session ingress endpoint read by the pinned binary"},
	{"BETA_TRACING_ENDPOINT", "network_wiring", "Claude CLI tracing endpoint read by the pinned binary"},
	{"SDK_NATIVE_BIN", "process_loader", "path of the native Claude binary the SDK launches"},
	{"AUTOMODE_DECISION_LOG", "harness_control", "Claude CLI auto-mode decision log path read by the pinned binary"},
	{"NODE_DEBUG", "harness_control", "Node debug log selector read by the Claude CLI runtime"},
	{"NODE_V8_COVERAGE", "process_loader", "Node coverage output directory read by the Claude CLI runtime"},
	{"NODE_REDIRECT_WARNINGS", "harness_control", "Node warning redirection file read by the Claude CLI runtime"},
	{"NODE_ICU_DATA", "process_loader", "Node ICU data path read by the Claude CLI runtime"},
	{"NODE_USE_ENV_PROXY", "network_wiring", "Node switch that routes built-in HTTP clients through *_PROXY variables, read by the Claude CLI runtime"},
	{"NODE_USE_SYSTEM_CA", "network_wiring", "Node switch that trusts the system CA store, read by the Claude CLI runtime"},
	{"NODE_COMPILE_CACHE", "managed_dirs", "Node compile-cache directory read by the Claude CLI runtime"},
	{"UV_THREADPOOL_SIZE", "process_loader", "libuv thread-pool size read by the Claude CLI runtime"},
	{"SSLKEYLOGFILE", "network_wiring", "TLS key log file read by the Node and Bun runtimes; would write session keys to a path the user picks"},
}

var deniedPrefixes = []deniedRule{
	{"UZI_", "worker_credentials", "worker-owned variables"},
	{"CLAUDE_", "provider_auth", "provider selection and credentials; includes every CLAUDE_CODE_USE_* selector"},
	{"ANTHROPIC_", "provider_auth", "provider selection and credentials; includes every CLAUDE_CODE_USE_* selector"},
	{"CODEX_", "provider_auth", "provider selection and credentials; includes every CLAUDE_CODE_USE_* selector"},
	{"OPENAI_", "provider_auth", "provider selection and credentials; includes every CLAUDE_CODE_USE_* selector"},
	{"MCP_", "harness_control", "MCP and feature switches read by the Claude CLI"},
	{"DISABLE_", "harness_control", "MCP and feature switches read by the Claude CLI"},
	{"GH_", "git_authority", "forge CLIs and tokens"},
	{"GITHUB_", "git_authority", "forge CLIs and tokens"},
	{"GITLAB_", "git_authority", "forge CLIs and tokens"},
	{"GLAB_", "git_authority", "forge CLIs and tokens"},
	{"GIT_", "git_authority", "git and ssh authority"},
	{"SSH_", "git_authority", "git and ssh authority"},
	{"LD_", "process_loader", "dynamic loader injection"},
	{"DYLD_", "process_loader", "dynamic loader injection"},
	{"BASH_", "shell_semantics", "bash behaviour variables"},
	{"XDG_", "managed_dirs", "XDG base directories under the run home"},
	{"SSL_CERT_", "network_wiring", "OpenSSL trust store"},
	{"PYTHON", "process_loader", "interpreter start-up and module search"},
	{"PERL5", "process_loader", "interpreter start-up and module search"},
	{"NIX_", "process_loader", "toolchain wrappers the worker image provides"},
	{"DEVBOX_", "process_loader", "toolchain wrappers the worker image provides"},
	{"BUN_", "process_loader", "Bun runtime configuration read by the Claude CLI binary (BUN_CONFIG_*, BUN_INSTALL_*)"},
	{"SELF_HOSTED_RUNNER_", "harness_control", "Claude CLI self-hosted runner mode settings (SELF_HOSTED_RUNNER_*), read by the pinned binary"},
	{"CCR_", "harness_control", "Claude CLI remote-session variables (CCR_*), read by the pinned binary"},
}

var deniedSuffixes = []deniedRule{
	{"_PROXY", "network_wiring", "changes network routing of every process"},
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
	for _, n := range deniedNames {
		if name == n.Value {
			return fmt.Sprintf("%s is set or reserved by the worker (denied name; protects %s)", name, n.Invariant)
		}
	}
	for _, p := range deniedPrefixes {
		if strings.HasPrefix(name, p.Value) {
			return fmt.Sprintf("denied prefix %s: names starting with it are reserved for the worker and the agent harness (protects %s)", p.Value, p.Invariant)
		}
	}
	for _, s := range deniedSuffixes {
		if strings.HasSuffix(name, s.Value) {
			return fmt.Sprintf("denied suffix %s: names ending in it change network routing and are reserved (protects %s)", s.Value, s.Invariant)
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
