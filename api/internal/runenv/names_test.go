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

// nameRuleInvariants is the closed set of worker invariants a denied entry may name.
var nameRuleInvariants = []string{
	"worker_credentials", "provider_auth", "harness_control", "git_authority",
	"managed_dirs", "network_wiring", "process_loader", "shell_semantics",
}

type nameRuleEntry struct {
	Value     string `json:"value"`
	Invariant string `json:"invariant"`
	Note      string `json:"note"`
}

func ruleValues(es []nameRuleEntry) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.Value
	}
	return out
}

type nameRulesFile struct {
	HarnessVersions struct {
		ClaudeAgentSDK string `json:"claude_agent_sdk"`
		Codex          string `json:"codex"`
	} `json:"harness_versions"`
	DeniedNames    []nameRuleEntry `json:"denied_names"`
	DeniedPrefixes []nameRuleEntry `json:"denied_prefixes"`
	DeniedSuffixes []nameRuleEntry `json:"denied_suffixes"`
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
		{"denied names", deniedNames, ruleValues(fx.DeniedNames)},
		{"denied prefixes", deniedPrefixes, ruleValues(fx.DeniedPrefixes)},
		{"denied suffixes", deniedSuffixes, ruleValues(fx.DeniedSuffixes)},
	} {
		if !slices.Equal(c.got, c.want) {
			t.Errorf("%s differ from fixtures/run-env/name-rules.json:\n go:      %v\n fixture: %v", c.what, c.got, c.want)
		}
	}
}

// Every denied entry names the worker invariant it protects (from the closed set) and says why.
func TestFixtureEntriesNameAnInvariantAndANote(t *testing.T) {
	fx := loadNameRules(t)
	seen := map[string]bool{}
	for _, group := range [][]nameRuleEntry{fx.DeniedNames, fx.DeniedPrefixes, fx.DeniedSuffixes} {
		for _, e := range group {
			if !slices.Contains(nameRuleInvariants, e.Invariant) {
				t.Errorf("entry %q has invariant %q, want one of %v", e.Value, e.Invariant, nameRuleInvariants)
			}
			if strings.TrimSpace(e.Note) == "" {
				t.Errorf("entry %q has no note", e.Value)
			}
			if seen[e.Value] {
				t.Errorf("entry %q appears twice", e.Value)
			}
			seen[e.Value] = true
		}
	}
	for _, inv := range nameRuleInvariants {
		found := false
		for _, group := range [][]nameRuleEntry{fx.DeniedNames, fx.DeniedPrefixes, fx.DeniedSuffixes} {
			for _, e := range group {
				found = found || e.Invariant == inv
			}
		}
		if !found {
			t.Errorf("no entry protects invariant %q", inv)
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
	for _, e := range fx.DeniedNames {
		n := e.Value
		if deniedBy(n) == "" {
			t.Errorf("denied name %q is not denied by the lists", n)
		}
		if err := ValidateName(n); err == nil {
			t.Errorf("ValidateName(%q) accepted a denied name", n)
		}
	}
	for _, e := range fx.DeniedPrefixes {
		p := e.Value
		n := p + "SOMETHING"
		if err := ValidateName(n); err == nil {
			t.Errorf("ValidateName(%q) accepted a name under denied prefix %q", n, p)
		}
	}
	for _, e := range fx.DeniedSuffixes {
		s := e.Value
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
		default:
			t.Errorf("overlap %s has decision %q", o.Name, o.Decision)
		}
		if strings.TrimSpace(o.Reason) == "" {
			t.Errorf("overlap %s has no reason", o.Name)
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
		{"ANT_OPTS", ""},
		{"ANT_HOME", ""},
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
		{"BUN_BE_BUN", "denied prefix BUN_"},
		{"BUN_CONFIG_VERBOSE_FETCH", "denied prefix BUN_"},
		{"NODE_DEBUG", "NODE_DEBUG is set or reserved"},
		{"NODE_V8_COVERAGE", "NODE_V8_COVERAGE is set or reserved"},
		{"NODE_REDIRECT_WARNINGS", "NODE_REDIRECT_WARNINGS is set or reserved"},
		{"NODE_ICU_DATA", "NODE_ICU_DATA is set or reserved"},
		{"NODE_USE_ENV_PROXY", "NODE_USE_ENV_PROXY is set or reserved"},
		{"NODE_COMPILE_CACHE", "NODE_COMPILE_CACHE is set or reserved"},
		{"UV_THREADPOOL_SIZE", "UV_THREADPOOL_SIZE is set or reserved"},
		{"SSLKEYLOGFILE", "SSLKEYLOGFILE is set or reserved"},
		{"ANT_OTEL_EXPORTER", "denied prefix ANT_OTEL_"},
		{"ANT_CLAUDE_CODE_METRICS_ENDPOINT", "ANT_CLAUDE_CODE_METRICS_ENDPOINT is set or reserved"},
		{"LC_ALL", "denied prefix LC_"},
		{"LC_CTYPE", "denied prefix LC_"},
		{"LANGUAGE", "LANGUAGE is set or reserved"},
		{"MIMALLOC_VERBOSE", "denied prefix MIMALLOC_"},
		{"_CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL", "denied prefix _CLAUDE_"},
		{"__CLAUDE_CC_KEEP_GOPROXY", "denied prefix __CLAUDE_"},
		{"CARES_HOSTS", "CARES_HOSTS is set or reserved"},
		{"HOSTALIASES", "HOSTALIASES is set or reserved"},
		{"LOCALDOMAIN", "LOCALDOMAIN is set or reserved"},
		{"RES_OPTIONS", "RES_OPTIONS is set or reserved"},
		{"NODE_ENV", ""},
		{"DEBUG", ""},
		{"NPM_CONFIG_REGISTRY", ""},
		{"PIP_INDEX_URL", ""},
		{"MAVEN_OPTS", ""},
		{"AWS_REGION", ""},
		{"GOOGLE_APPLICATION_CREDENTIALS", ""},
		{"AZURE_CLIENT_SECRET", ""},
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
