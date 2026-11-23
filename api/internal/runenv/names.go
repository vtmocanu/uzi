package runenv

import (
	"fmt"
	"regexp"
	"strings"
)

// The three lists below are the Go copy of fixtures/run-env/name-rules.json, which
// also records the harness versions they were derived from and which the worker
// pins as well. names_test.go fails when the copy and the fixture differ.

// deniedNames are exact names a variable may not use: the worker's own invariants
// and guardrails (its credentials, Docker endpoint, git authority, provider
// configuration and managed directories) plus the harness controls outside the
// denied prefixes.
var deniedNames = []string{
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
}

var deniedPrefixes = []string{
	"UZI_", "CLAUDE_", "ANTHROPIC_", "CODEX_", "OPENAI_", "MCP_", "DISABLE_",
	"GH_", "GITHUB_", "GITLAB_", "GLAB_",
	"GIT_", "SSH_",
	"LD_", "DYLD_", "BASH_", "XDG_", "SSL_CERT_", "PYTHON", "PERL5", "NIX_", "DEVBOX_", "BUN_",
}

var deniedSuffixes = []string{"_PROXY"}

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
		if name == n {
			return fmt.Sprintf("%s is set or reserved by the worker (denied name)", name)
		}
	}
	for _, p := range deniedPrefixes {
		if strings.HasPrefix(name, p) {
			return fmt.Sprintf("denied prefix %s: names starting with it are reserved for the worker and the agent harness", p)
		}
	}
	for _, s := range deniedSuffixes {
		if strings.HasSuffix(name, s) {
			return fmt.Sprintf("denied suffix %s: names ending in it change network routing and are reserved", s)
		}
	}
	return ""
}

// ValidateNameShape applies only the name grammar, not the denylist. Disable and
// delete use it so a variable stored under a name that was denied later can still
// be cut off. A non-nil result is a *RuleError.
func ValidateNameShape(name string) error {
	if !NameRegexp.MatchString(name) {
		return &RuleError{Field: "name", Rule: "must match ^[A-Z_][A-Z0-9_]{0,127}$ (upper-case letters, digits and underscores, not starting with a digit, at most 128 characters)"}
	}
	return nil
}

// ValidateName applies the name shape and the denylist. A non-nil result is a
// *RuleError whose message names the broken rule.
func ValidateName(name string) error {
	if err := ValidateNameShape(name); err != nil {
		return err
	}
	if rule := deniedBy(name); rule != "" {
		return &RuleError{Field: "name", Rule: rule}
	}
	return nil
}
