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

// TestDeniedExecutablesCoverDenylist pins the two maps together. denylist is keyed by
// PACKAGE and deniedPackageExecutables maps package -> executables; anything observing
// a shell can only see the executable, so a gap here silently narrows DeniedExecutable
// without failing anything else.
//
// It asserts BOTH directions on purpose. Missing-package is the drift that matters
// (a new denylist entry whose CLI stays observable), but an extra entry here means a
// package was dropped from the denylist and this map kept claiming to bar it.
func TestDeniedExecutablesCoverDenylist(t *testing.T) {
	for pkg := range denylist {
		execs, ok := deniedPackageExecutables[pkg]
		if !ok {
			t.Errorf("denylisted package %q has no executables mapped: DeniedExecutable cannot see its CLI", pkg)
			continue
		}
		if len(execs) == 0 {
			t.Errorf("denylisted package %q maps to an empty executable list", pkg)
		}
	}
	for pkg := range deniedPackageExecutables {
		if !denylist[pkg] {
			t.Errorf("deniedPackageExecutables names %q, which is not on the denylist", pkg)
		}
	}
}

// TestDeniedExecutableCoversDivergentNames is the case a name-equality check fails.
// The package is `awscli`/`azure-cli`/`google-cloud-sdk`; the command a shell reports
// missing is `aws`/`az`/`gcloud`. Denied() answers about the former and would say no
// to all three of the latter.
func TestDeniedExecutableCoversDivergentNames(t *testing.T) {
	for _, cmd := range []string{"glab", "gh", "aws", "az", "gcloud", "gsutil", "bq", "sam", "oci", "fly"} {
		if !DeniedExecutable(cmd) {
			t.Errorf("DeniedExecutable(%q) = false, want true", cmd)
		}
	}
	// Sanity in the other direction: an ordinary tool must stay reportable, or the
	// suppression would swallow real missing-tool findings.
	for _, cmd := range []string{"file", "perl", "fmt", "helm", "kubeconform", "jq", "go"} {
		if DeniedExecutable(cmd) {
			t.Errorf("DeniedExecutable(%q) = true, want false", cmd)
		}
	}
	// Denied() takes a package and genuinely does NOT answer for these, which is why
	// DeniedExecutable exists rather than reusing it.
	if Denied("aws") || Denied("az") {
		t.Error("Denied() unexpectedly matched an executable name; the divergence this test documents has changed")
	}
}
