package providers

import (
	"encoding/json"
	"os"
	"testing"
)

const identityFixture = "../../../fixtures/provider-model-identity/cases.json"

type identityCase struct {
	Name      string   `json:"name"`
	Engine    string   `json:"engine"`
	Map       ModelMap `json:"map"`
	Requested string   `json:"requested"`
	Explicit  bool     `json:"explicit"`
	Resolved  *string  `json:"resolved"`
}

func TestResolveModelIDFixture(t *testing.T) {
	raw, err := os.ReadFile(identityFixture)
	if err != nil {
		t.Fatal(err)
	}
	var cases []identityCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	const wantCases = 14
	if len(cases) != wantCases {
		t.Fatalf("fixture has %d cases, want %d", len(cases), wantCases)
	}
	seen := map[string]bool{}
	for _, c := range cases {
		if seen[c.Name] {
			t.Fatalf("duplicate case name %q", c.Name)
		}
		seen[c.Name] = true
		t.Run(c.Name, func(t *testing.T) {
			got, ok := ResolveModelID(c.Engine, c.Map, c.Requested, c.Explicit)
			if c.Resolved == nil {
				if ok {
					t.Fatalf("got (%q, true), want unresolved", got)
				}
				return
			}
			if !ok || got != *c.Resolved {
				t.Fatalf("got (%q, %v), want %q", got, ok, *c.Resolved)
			}
		})
	}
	for _, name := range []string{
		"claude alias present", "claude alias absent", "claude target value maps to itself",
		"claude non-alias value matching no target", "claude blank goes through sonnet",
		"claude inherit goes through sonnet", "codex key with a differing target",
		"codex key absent and not explicit gives default", "codex key absent and explicit is unresolved",
		"codex blank gives default", "codex requested default literal with explicit",
		"claude whitespace-only goes through sonnet", "codex explicit blank is unresolved",
		"codex requested default literal without explicit",
	} {
		if !seen[name] {
			t.Errorf("fixture case %q was not exercised", name)
		}
	}
}

// An empty target in the map is Unresolved (fail closed). Go-only: the agent treats it differently by
// design and the map validator refuses empty targets, so it is not a shared fixture case.
func TestResolveModelIDEmptyTargetFailsClosed(t *testing.T) {
	m := ModelMap{"default": {Model: ""}, "sonnet": {Model: ""}}
	if got, ok := ResolveModelID(EngineCodex, m, "default", false); ok {
		t.Errorf("codex empty target: got (%q, true), want unresolved", got)
	}
	if got, ok := ResolveModelID(EngineClaude, m, "sonnet", false); ok {
		t.Errorf("claude empty target: got (%q, true), want unresolved", got)
	}
}

func TestNormalizedHost(t *testing.T) {
	for _, c := range []struct {
		in, want string
		err      bool
	}{
		{"https://gw.example.com/v1", "gw.example.com", false},
		{"https://GW.Example.com:8443/v1/", "gw.example.com", false},
		{"https://gw.example.com./v1", "", true},
		{"", "", true},
		{"https://user@gw.example.com", "", true},
	} {
		got, err := NormalizedHost(c.in)
		if (err != nil) != c.err || got != c.want {
			t.Errorf("NormalizedHost(%q) = (%q, %v), want (%q, err=%v)", c.in, got, err, c.want, c.err)
		}
	}
}

func TestCompareCheckerIdentity(t *testing.T) {
	prov := func(host, id string) IdentitySide {
		return IdentitySide{ProviderBound: true, Host: host, ModelID: id, Resolved: true}
	}
	vendor := func(id string) IdentitySide { return IdentitySide{ModelID: id, Resolved: true} }
	unres := func(s IdentitySide) IdentitySide { s.Resolved = false; return s }
	for _, c := range []struct {
		name          string
		lead, checker IdentitySide
		want          IdentityOutcome
	}{
		{"vendor/vendor same id", vendor("m"), vendor("m"), NotEvaluated},
		{"vendor/vendor unresolved", unres(vendor("")), vendor("m"), NotEvaluated},
		{"provider/provider same host same id", prov("h", "m"), prov("h", "m"), SameModel},
		{"provider/provider different host same id", prov("h1", "m"), prov("h2", "m"), Independent},
		{"provider/provider same host different id", prov("h", "a"), prov("h", "b"), Independent},
		{"vendor lead provider checker same id", vendor("m"), prov("h", "m"), SameModel},
		{"provider lead vendor checker same id", prov("h", "m"), vendor("m"), SameModel},
		{"vendor lead provider checker different id", vendor("a"), prov("h", "b"), Independent},
		{"provider lead vendor checker different id", prov("h", "a"), vendor("b"), Independent},
		{"case is not folded", vendor("Opus"), prov("h", "opus"), Independent},
		{"unresolved lead", unres(prov("h", "m")), prov("h", "m"), Unresolved},
		{"unresolved checker", prov("h", "m"), unres(prov("h", "m")), Unresolved},
		{"unresolved vendor lead with provider checker", unres(vendor("")), prov("h", "m"), Unresolved},
		{"provider lead with empty host fails closed", prov("", "m"), prov("h", "m"), Unresolved},
		{"provider checker with empty host fails closed", vendor("m"), prov("", "m"), Unresolved},
	} {
		if got := CompareCheckerIdentity(c.lead, c.checker); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
