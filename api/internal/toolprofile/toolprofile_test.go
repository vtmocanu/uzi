package toolprofile

import (
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// rules used across the tests: kubectl any-version, terraform pinned to 1.7, jq
// any-version.
var testRules = Rules{
	"kubectl":   {},
	"terraform": {PinnedVersion: "1.7"},
	"jq":        {},
}

func TestAllowed(t *testing.T) {
	ok := []string{"kubectl", "kubectl@1.31", "jq", "terraform@1.7"}
	for _, p := range ok {
		if !Allowed(p, testRules) {
			t.Errorf("Allowed(%q) = false, want true", p)
		}
	}
	bad := []string{
		"", "not-a-real-pkg", "kubectl; rm -rf /", "../etc", "kubectl kubectl",
		"$(whoami)", "jq&", "kube ctl", "kubectl@", "@1.0",
		"terraform",     // pinned rule requires @1.7
		"terraform@1.6", // wrong pinned version
		"terraform@",    // malformed
	}
	for _, p := range bad {
		if Allowed(p, testRules) {
			t.Errorf("Allowed(%q) = true, want false", p)
		}
	}
}

func TestWellFormed(t *testing.T) {
	for _, ok := range []string{"kubectl", "kubectl@1.31", "go", "python3", "a.b_c+d"} {
		if !WellFormed(ok) {
			t.Errorf("WellFormed(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", " ", "a b", "../x", "a;b", "a@"} {
		if WellFormed(bad) {
			t.Errorf("WellFormed(%q) = true, want false", bad)
		}
	}
}

func TestResolveFiltersAndDedups(t *testing.T) {
	allowed, rejected := Resolve([]string{"kubectl@1.31", "evil; rm", "jq", "kubectl@1.31", "  terraform@1.7  ", "nope", "terraform@9"}, testRules)
	if !reflect.DeepEqual(allowed, []string{"jq", "kubectl@1.31", "terraform@1.7"}) {
		t.Fatalf("allowed = %v, want [jq kubectl@1.31 terraform@1.7]", allowed)
	}
	if !reflect.DeepEqual(rejected, []string{"evil; rm", "nope", "terraform@9"}) {
		t.Fatalf("rejected = %v, want [evil; rm nope terraform@9]", rejected)
	}
}

func TestResolveEmptyIsNil(t *testing.T) {
	allowed, rejected := Resolve(nil, testRules)
	if allowed != nil || rejected != nil {
		t.Fatalf("Resolve(nil) = (%v, %v), want (nil, nil)", allowed, rejected)
	}
}

func TestDeniedCredentialClIsRejectedEvenIfAllowlisted(t *testing.T) {
	// Decision 6: a credential-bearing CLI is barred even if an admin allowlists it.
	rules := Rules{"glab": {}, "gh": {}, "kubectl": {}}
	for _, denied := range []string{"glab", "gh", "gh@2.0", "awscli", "gcloud", "vault"} {
		if !Denied(denied) {
			t.Errorf("Denied(%q) = false, want true", denied)
		}
		if Allowed(denied, rules) {
			t.Errorf("Allowed(%q) = true despite being on the denylist", denied)
		}
	}
	// A non-denied allowlisted package still passes.
	if !Allowed("kubectl", rules) {
		t.Error("kubectl should be allowed (not denied)")
	}
}

func TestWellFormedLengthCap(t *testing.T) {
	long := strings.Repeat("a", maxPkgLen+1)
	if WellFormed(long) {
		t.Errorf("WellFormed(%d chars) = true, want false (over cap)", len(long))
	}
	if WellFormedVersion(strings.Repeat("1", maxPkgLen+1)) {
		t.Error("over-length version should be rejected")
	}
	if !WellFormed(strings.Repeat("a", maxPkgLen)) {
		t.Error("a package exactly at the cap should be allowed")
	}
}

func TestRulesFromRows(t *testing.T) {
	rows := []store.ToolAllowlist{
		{Name: "kubectl"},
		{Name: "terraform", PinnedVersion: pgtype.Text{String: "1.7", Valid: true}},
	}
	rules := RulesFromRows(rows)
	if len(rules) != 2 {
		t.Fatalf("rules len = %d, want 2", len(rules))
	}
	if rules["kubectl"].PinnedVersion != "" {
		t.Errorf("kubectl should have no pinned version, got %q", rules["kubectl"].PinnedVersion)
	}
	if rules["terraform"].PinnedVersion != "1.7" {
		t.Errorf("terraform pinned = %q, want 1.7", rules["terraform"].PinnedVersion)
	}
	// The projected rules drive Resolve identically at write + claim time.
	allowed, rejected := Resolve([]string{"kubectl", "terraform@1.7", "terraform@9"}, rules)
	if !reflect.DeepEqual(allowed, []string{"kubectl", "terraform@1.7"}) {
		t.Fatalf("allowed = %v", allowed)
	}
	if !reflect.DeepEqual(rejected, []string{"terraform@9"}) {
		t.Fatalf("rejected = %v", rejected)
	}
}

func TestSplit(t *testing.T) {
	for _, tc := range []struct{ in, base, ver string }{
		{"kubectl", "kubectl", ""},
		{"kubectl@1.31", "kubectl", "1.31"},
		{"go@1.22.0", "go", "1.22.0"},
	} {
		b, v := Split(tc.in)
		if b != tc.base || v != tc.ver {
			t.Errorf("Split(%q) = (%q,%q), want (%q,%q)", tc.in, b, v, tc.base, tc.ver)
		}
	}
}
