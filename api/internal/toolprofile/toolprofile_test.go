package toolprofile

import (
	"reflect"
	"testing"
)

func TestAllowed(t *testing.T) {
	// On the allowlist, with and without a version pin.
	for _, ok := range []string{"kubectl", "kubectl@1.31", "terraform", "jq", "go@1.22"} {
		if !Allowed(ok) {
			t.Errorf("Allowed(%q) = false, want true", ok)
		}
	}
	// Off the allowlist, or malformed (shell metachars / paths / spaces).
	for _, bad := range []string{
		"", "not-a-real-pkg", "kubectl; rm -rf /", "../etc", "kubectl kubectl",
		"$(whoami)", "jq&", "kube ctl", "kubectl@", "@1.0",
	} {
		if Allowed(bad) {
			t.Errorf("Allowed(%q) = true, want false", bad)
		}
	}
}

func TestResolveFiltersAndDedups(t *testing.T) {
	allowed, rejected := Resolve([]string{"kubectl@1.31", "evil; rm", "jq", "kubectl@1.31", "  terraform  ", "nope"})
	// Kept: deduped + sorted; the version pin is preserved.
	if !reflect.DeepEqual(allowed, []string{"jq", "kubectl@1.31", "terraform"}) {
		t.Fatalf("allowed = %v, want [jq kubectl@1.31 terraform]", allowed)
	}
	// Rejected: the disallowed/malformed entries, in input order.
	if !reflect.DeepEqual(rejected, []string{"evil; rm", "nope"}) {
		t.Fatalf("rejected = %v, want [evil; rm nope]", rejected)
	}
}

func TestResolveEmptyIsNil(t *testing.T) {
	// The M3 default: no desired packages ⇒ nothing to provision.
	allowed, rejected := Resolve(nil)
	if allowed != nil || rejected != nil {
		t.Fatalf("Resolve(nil) = (%v, %v), want (nil, nil)", allowed, rejected)
	}
}
