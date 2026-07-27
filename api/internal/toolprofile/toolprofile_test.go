package toolprofile

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
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
//
// KNOWN LIMIT, stated so nobody mistakes this for more than it is: this checks that
// you wrote SOMETHING, not that you wrote the RIGHT thing. A denylist entry mapped to
// a misspelled binary passes here (mutation-verified). Only the derivation knows the
// true bin set; short of evaluating nixpkgs in a unit test, the real guard is review.
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
		for _, e := range execs {
			if strings.TrimSpace(e) == "" {
				t.Errorf("denylisted package %q maps an empty executable name", pkg)
			}
		}
	}
	for pkg := range deniedPackageExecutables {
		if !denylist[pkg] {
			t.Errorf("deniedPackageExecutables names %q, which is not on the denylist", pkg)
		}
	}
}

// TestDeniedExecutablesAreNotInstallablePackages is a REGRESSION PIN for the `fly`
// defect, not a guard — the distinction matters and the first version of this comment
// blurred it. flyctl symlinks its binary to `fly`, but nixpkgs `fly` is the Concourse CI
// client: a different, NOT-denylisted, installable tool whose CLI the alias made
// permanently unreportable.
//
// It cannot DISCOVER a collision, only re-catch the one already found: with `fly`
// removed, zero entries below are reachable from deniedExecutableSet, so the loop's
// true branch never fires today. Verified live — re-adding `fly` to flyctl's list
// reddens it. Keep it for that, and read the next test for the check that is actually
// data-driven.
func TestDeniedExecutablesAreNotInstallablePackages(t *testing.T) {
	for exec := range deniedExecutableSet {
		if knownInstallablePackages[exec] && !denylist[exec] {
			t.Errorf("executable %q is suppressed but names an installable, non-denylisted package: "+
				"a real missing-tool finding for it could never be reported", exec)
		}
	}
}

// knownInstallablePackages are nixpkgs attribute names that collide with an executable
// some denylisted package installs. Hand-maintained and deliberately short: it pins
// collisions already found, and makes no claim to mirror nixpkgs. Verified at the rev
// agent/devbox-global/devbox.lock pins.
var knownInstallablePackages = map[string]bool{
	"fly": true, // "Command line interface to Concourse CI" - NOT flyctl
}

// TestDeniedExecutablesDoNotShadowTheBakedToolchain is the data-driven half. Rather
// than a list someone must think to update, it reads the packages actually baked into
// every worker image and asserts none of them is suppressed.
//
// Why this one bites when the pin above cannot: the baked manifest changes for its own
// reasons — five packages were added to it the same day this test was written — so a
// future collision arrives WITHOUT anyone editing this file, which is exactly the case
// a hand-maintained list misses. If the toolchain ever bakes a tool whose name collides
// with a denied CLI, a genuine missing-tool finding for it would be unreportable.
func TestDeniedExecutablesDoNotShadowTheBakedToolchain(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "agent", "devbox-global", "devbox.json"))
	if err != nil {
		t.Fatalf("read baked toolchain manifest: %v", err)
	}
	// devbox.json is HuJSON (it carries // comments), so pull the packages array by
	// shape rather than unmarshalling.
	m := regexp.MustCompile(`(?s)"packages"\s*:\s*\[(.*?)\]`).FindSubmatch(raw)
	if m == nil {
		t.Fatal("could not find the packages array in devbox.json — did its shape change?")
	}
	body := regexp.MustCompile(`//[^\n]*`).ReplaceAll(m[1], nil)
	pkgs := regexp.MustCompile(`"([^"]+)"`).FindAllSubmatch(body, -1)
	if len(pkgs) < 5 {
		t.Fatalf("parsed only %d baked packages — the scan is probably broken, not the manifest", len(pkgs))
	}
	for _, p := range pkgs {
		name := string(p[1])
		// A baked package name may carry an output suffix (`openssl.bin`, `file.out`);
		// the executable is what collides, so compare on the base attr too.
		base := name
		if i := strings.IndexByte(base, '.'); i > 0 {
			base = base[:i]
		}
		if deniedExecutableSet[name] || deniedExecutableSet[base] {
			t.Errorf("baked toolchain package %q collides with a suppressed executable: "+
				"a genuine missing-tool finding for it could never be reported", name)
		}
	}
}

// TestDeniedExecutableCoversDivergentNames is the case a name-equality check fails.
// The package is `awscli`/`azure-cli`/`google-cloud-sdk`; the command a shell reports
// missing is `aws`/`az`/`gcloud`.
func TestDeniedExecutableCoversDivergentNames(t *testing.T) {
	for _, cmd := range []string{
		"glab", "gh", "aws", "az", "gcloud", "gsutil", "bq", "sam", "oci",
		"git-credential-gcloud.sh", "docker-credential-gcloud", "op", "bw",
	} {
		if !DeniedExecutable(cmd) {
			t.Errorf("DeniedExecutable(%q) = false, want true", cmd)
		}
	}
	// Path forms: the exec.LookPath error the judge scan matches carries a full path,
	// so a bare map lookup misses it. Measured as a real bypass before basenaming.
	for _, cmd := range []string{"/usr/local/bin/glab", "./glab", "/usr/bin/aws"} {
		if !DeniedExecutable(cmd) {
			t.Errorf("DeniedExecutable(%q) = false, want true (path form must be basenamed)", cmd)
		}
	}
	// Ordinary tools must stay reportable, or the suppression swallows real findings.
	// `fly` is here deliberately: it is Concourse CI, not flyctl.
	for _, cmd := range []string{"file", "perl", "fmt", "helm", "kubeconform", "jq", "go", "fly"} {
		if DeniedExecutable(cmd) {
			t.Errorf("DeniedExecutable(%q) = true, want false", cmd)
		}
	}
}
