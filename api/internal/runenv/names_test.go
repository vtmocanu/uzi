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

type fixtureRule struct {
	Value     string `json:"value"`
	Invariant string `json:"invariant"`
	Note      string `json:"note"`
}

type nameRulesFile struct {
	Review struct {
		Date   string `json:"date"`
		Method string `json:"method"`
	} `json:"review"`
	Invariants      []string `json:"invariants"`
	HarnessVersions struct {
		ClaudeAgentSDK string `json:"claude_agent_sdk"`
		Codex          string `json:"codex"`
	} `json:"harness_versions"`
	DeniedNames    []fixtureRule `json:"denied_names"`
	DeniedPrefixes []fixtureRule `json:"denied_prefixes"`
	DeniedSuffixes []fixtureRule `json:"denied_suffixes"`
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
		what string
		got  []deniedRule
		want []fixtureRule
	}{
		{"denied names", deniedNames, fx.DeniedNames},
		{"denied prefixes", deniedPrefixes, fx.DeniedPrefixes},
		{"denied suffixes", deniedSuffixes, fx.DeniedSuffixes},
	} {
		if len(c.got) != len(c.want) {
			t.Errorf("%s: %d entries in Go, %d in fixtures/run-env/name-rules.json", c.what, len(c.got), len(c.want))
			continue
		}
		for i := range c.got {
			g := fixtureRule{c.got[i].Value, c.got[i].Invariant, c.got[i].Note}
			if g != c.want[i] {
				t.Errorf("%s differ from fixtures/run-env/name-rules.json at index %d:\n go:      %+v\n fixture: %+v", c.what, i, g, c.want[i])
				break
			}
		}
	}
}

func TestFixtureEntriesCarryAnInvariantAndNote(t *testing.T) {
	fx := loadNameRules(t)
	if fx.Review.Date == "" || strings.TrimSpace(fx.Review.Method) == "" {
		t.Error("the fixture records no review date or method")
	}
	for _, list := range [][]fixtureRule{fx.DeniedNames, fx.DeniedPrefixes, fx.DeniedSuffixes} {
		for _, r := range list {
			if !slices.Contains(fx.Invariants, r.Invariant) {
				t.Errorf("%q has invariant %q, not in %v", r.Value, r.Invariant, fx.Invariants)
			}
			if strings.TrimSpace(r.Note) == "" {
				t.Errorf("%q has no note", r.Value)
			}
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
	for _, r := range fx.DeniedNames {
		n := r.Value
		if deniedBy(n) == "" {
			t.Errorf("denied name %q is not denied by the lists", n)
		}
		if err := ValidateName(n); err == nil {
			t.Errorf("ValidateName(%q) accepted a denied name", n)
		}
	}
	for _, r := range fx.DeniedPrefixes {
		n := r.Value + "SOMETHING"
		if err := ValidateName(n); err == nil {
			t.Errorf("ValidateName(%q) accepted a name under denied prefix %q", n, r.Value)
		}
	}
	for _, r := range fx.DeniedSuffixes {
		n := "SOMETHING" + r.Value
		if err := ValidateName(n); err == nil {
			t.Errorf("ValidateName(%q) accepted a name with denied suffix %q", n, r.Value)
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
		{"BUN_CONFIG_REGISTRY", "denied prefix BUN_"},
		{"NODE_DEBUG", "NODE_DEBUG is set or reserved"},
		{"SSLKEYLOGFILE", "protects network_wiring"},
		{"NODE_ENV", ""},
		{"NPM_CONFIG_REGISTRY", ""},
		{"PIP_INDEX_URL", ""},
		{"MAVEN_OPTS", ""},
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
