package runenv

import (
	"encoding/json"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

const nameRulesFixture = "../../../fixtures/run-env/name-rules.json"

type nameRulesFile struct {
	HarnessVersions struct {
		ClaudeAgentSDK string `json:"claude_agent_sdk"`
		Codex          string `json:"codex"`
	} `json:"harness_versions"`
	DeniedNames    []string `json:"denied_names"`
	DeniedPrefixes []string `json:"denied_prefixes"`
	DeniedSuffixes []string `json:"denied_suffixes"`
	Overlaps       []struct {
		Name     string `json:"name"`
		Harness  string `json:"harness"`
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	} `json:"overlaps"`
}

func loadNameRules(t *testing.T) nameRulesFile {
	t.Helper()
	raw, err := os.ReadFile(nameRulesFixture)
	if err != nil {
		t.Fatal(err)
	}
	var fx nameRulesFile
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&fx); err != nil {
		t.Fatal(err)
	}
	return fx
}

// The Go lists are a copy of the shared fixture: any drift fails the build, the same
// way the worker's copy is pinned on the agent side.
func TestGoNameRulesEqualSharedFixture(t *testing.T) {
	fx := loadNameRules(t)
	for _, c := range []struct {
		what      string
		got, want []string
	}{
		{"denied names", deniedNames, fx.DeniedNames},
		{"denied prefixes", deniedPrefixes, fx.DeniedPrefixes},
		{"denied suffixes", deniedSuffixes, fx.DeniedSuffixes},
	} {
		if !slices.Equal(c.got, c.want) {
			t.Errorf("%s differ from fixtures/run-env/name-rules.json:\n go:      %v\n fixture: %v", c.what, c.got, c.want)
		}
	}
}

// The fixture records the harness versions it was derived from. A harness bump must
// update the fixture in the same change, after listing the new version's environment
// settings; the recorded version is never advanced only to make this pass.
func TestFixtureHarnessVersionsMatchPins(t *testing.T) {
	fx := loadNameRules(t)
	pkg, err := os.ReadFile("../../../agent/package.json")
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Dependencies map[string]string `json:"dependencies"`
	}
	if err := json.Unmarshal(pkg, &parsed); err != nil {
		t.Fatal(err)
	}
	if got := parsed.Dependencies["@anthropic-ai/claude-agent-sdk"]; got != fx.HarnessVersions.ClaudeAgentSDK {
		t.Errorf("agent/package.json pins @anthropic-ai/claude-agent-sdk %q, fixture records %q: review the new version's environment settings and update fixtures/run-env/name-rules.json", got, fx.HarnessVersions.ClaudeAgentSDK)
	}
	lock, err := os.ReadFile("../../../agent/codex/codex-package.lock")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^CODEX_VERSION=(\S+)$`).FindSubmatch(lock)
	if m == nil {
		t.Fatal("CODEX_VERSION not found in agent/codex/codex-package.lock")
	}
	if got := string(m[1]); got != fx.HarnessVersions.Codex {
		t.Errorf("codex-package.lock pins CODEX_VERSION %q, fixture records %q: review the new version's environment settings and update fixtures/run-env/name-rules.json", got, fx.HarnessVersions.Codex)
	}
}

func TestFixtureNamesAreDeniedByTheLists(t *testing.T) {
	fx := loadNameRules(t)
	for _, n := range fx.DeniedNames {
		if deniedBy(n) == "" {
			t.Errorf("denied name %q is not denied by the lists", n)
		}
		if !NameRegexp.MatchString(n) {
			// npm_config_cache fails the name grammar, which rejects it before any list is consulted.
			continue
		}
		// An exact denied name must be rejected by the exact-name rule, not by a prefix or the grammar.
		if err := ValidateName(n); err == nil || !strings.Contains(err.Error(), "(denied name)") {
			t.Errorf("ValidateName(%q) = %v, want the exact denied-name rule", n, err)
		}
	}
	for _, p := range fx.DeniedPrefixes {
		n := p + "SOMETHING"
		if err := ValidateName(n); err == nil || !strings.Contains(err.Error(), "denied prefix "+p) {
			t.Errorf("ValidateName(%q) = %v, want the denied prefix %q rule", n, err, p)
		}
	}
	for _, s := range fx.DeniedSuffixes {
		n := "SOMETHING" + s
		if err := ValidateName(n); err == nil {
			t.Errorf("ValidateName(%q) accepted a name with denied suffix %q", n, s)
		}
	}
}

func TestFixtureOverlapDecisions(t *testing.T) {
	fx := loadNameRules(t)
	if len(fx.Overlaps) == 0 {
		t.Fatal("the fixture records no overlaps")
	}
	for _, o := range fx.Overlaps {
		err := ValidateName(o.Name)
		switch o.Decision {
		case "allowed":
			if err != nil {
				t.Errorf("overlap %s (%s) is recorded as allowed but refused: %v", o.Name, o.Harness, err)
			}
		case "denied":
			if err == nil {
				t.Errorf("overlap %s (%s) is recorded as denied but accepted", o.Name, o.Harness)
			}
			// A reserved rule must deny it, not merely the name grammar.
			if deniedBy(o.Name) == "" {
				t.Errorf("overlap %s (%s) is recorded as denied but no reserved rule denies it", o.Name, o.Harness)
			}
		default:
			t.Errorf("overlap %s has decision %q", o.Name, o.Decision)
		}
		if strings.TrimSpace(o.Reason) == "" {
			t.Errorf("overlap %s has no reason", o.Name)
		}
	}
}

func TestValidateNameShape(t *testing.T) {
	for _, n := range []string{"TMPDIR", "UZI_X", "HTTP_PROXY", "DATABASE_URL"} {
		if err := ValidateNameShape(n); err != nil {
			t.Errorf("ValidateNameShape(%q) = %v, want accepted", n, err)
		}
	}
	for _, n := range []string{"SystemDrive", "Temp", "1A", "", strings.Repeat("A", 129)} {
		err := ValidateNameShape(n)
		if err == nil || !strings.Contains(err.Error(), "^[A-Z_][A-Z0-9_]{0,127}$") {
			t.Errorf("ValidateNameShape(%q) = %v, want the grammar error", n, err)
		}
	}
	if err := ValidateName("TMPDIR"); err == nil {
		t.Error("ValidateName(TMPDIR) accepted a denied name")
	}
}

// The Windows spellings the Claude CLI reads (SystemDrive, Temp, Tmp) stay rejected by the
// grammar; only the uppercase API spellings are reserved names, and the TypeScript
// watcher preferences are ordinary application configuration.
func TestWindowsSpellingsAndTscNames(t *testing.T) {
	for _, n := range []string{"SystemDrive", "Temp", "Tmp"} {
		err := ValidateName(n)
		if err == nil || !strings.Contains(err.Error(), "must match") {
			t.Errorf("ValidateName(%q) = %v, want the grammar error", n, err)
		}
	}
	for _, n := range []string{"TEMP", "TMP", "SYSTEMDRIVE", "NODE_INSPECTOR_IPC", "VSCODE_INSPECTOR_OPTIONS",
		"API_FORCE_IDLE_TIMEOUT", "ENABLE_TOOL_SEARCH", "SLASH_COMMAND_TOOL_CHAR_BUDGET", "AUTOMODE_DECISION_LOG",
		"NODE_USE_SYSTEM_CA", "ENABLE_BETA_TRACING_DETAILED", "ENABLE_ENHANCED_TELEMETRY_BETA", "BETA_TRACING_ENDPOINT",
		"_CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL"} {
		err := ValidateName(n)
		if err == nil || !strings.Contains(err.Error(), "(denied name)") {
			t.Errorf("ValidateName(%q) = %v, want the denied-name rule", n, err)
		}
	}
	for _, n := range []string{"TSC_WATCHFILE", "TSC_WATCHDIRECTORY", "TSC_NONPOLLING_WATCHER"} {
		if err := ValidateName(n); err != nil {
			t.Errorf("ValidateName(%q) = %v, want accepted", n, err)
		}
	}
}

func TestValidateNameTable(t *testing.T) {
	tests := []struct {
		name    string
		wantErr string // substring of the rule, "" = accepted
	}{
		{"DATABASE_URL", ""},
		{"STRIPE_API_KEY", ""},
		{"SENTRY_DSN", ""},
		{"AWS_SECRET_ACCESS_KEY", ""},
		{"NPM_TOKEN", ""},
		{"GRADLE_OPTS", ""},
		{"ORG_GRADLE_PROJECT_X", ""},
		{"TESTCONTAINERS_HOST_OVERRIDE", ""},
		{"DOCKER_AUTH_CONFIG", ""},
		{"_UNDERSCORE_FIRST", ""},
		{"A", ""},
		{strings.Repeat("A", 128), ""},
		{strings.Repeat("A", 129), "^[A-Z_][A-Z0-9_]{0,127}$"},
		{"", "^[A-Z_][A-Z0-9_]{0,127}$"},
		{"1STARTS_WITH_DIGIT", "^[A-Z_][A-Z0-9_]{0,127}$"},
		{"lower_case", "^[A-Z_][A-Z0-9_]{0,127}$"},
		{"HAS-DASH", "^[A-Z_][A-Z0-9_]{0,127}$"},
		{"HAS SPACE", "^[A-Z_][A-Z0-9_]{0,127}$"},
		{"ANTHROPIC_BASE_URL", "denied prefix ANTHROPIC_"},
		{"NODE_OPTIONS", "NODE_OPTIONS is set or reserved"},
		{"GITHUB_TOKEN", "denied prefix GITHUB_"},
		{"PATH", "PATH is set or reserved"},
		{"HTTP_PROXY", "denied suffix _PROXY"},
		{"NO_PROXY", "denied suffix _PROXY"},
		{"PYTHONPATH", "denied prefix PYTHON"},
		{"DISABLE_AUTOUPDATER", "denied prefix DISABLE_"},
		{"CLAUDE_CODE_USE_VERTEX", "denied prefix CLAUDE_"},
		{"API_TIMEOUT_MS", "API_TIMEOUT_MS is set or reserved"},
		{"MAX_THINKING_TOKENS", "MAX_THINKING_TOKENS is set or reserved"},
		{"MAX_MCP_OUTPUT_TOKENS", "MAX_MCP_OUTPUT_TOKENS is set or reserved"},
		{"DOCKER_HOST", "DOCKER_HOST is set or reserved"},
		{"UZI_ANYTHING", "denied prefix UZI_"},
	}
	for _, tc := range tests {
		err := ValidateName(tc.name)
		if tc.wantErr == "" {
			if err != nil {
				t.Errorf("ValidateName(%q) = %v, want accepted", tc.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("ValidateName(%q) = %v, want an error naming %q", tc.name, err, tc.wantErr)
		}
	}
}
