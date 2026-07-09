package workertmpl

import "testing"

func TestValid(t *testing.T) {
	for _, name := range Names {
		if !Valid(name) {
			t.Errorf("Valid(%q) = false, want true (it is in Names)", name)
		}
	}
	for _, bad := range []string{"", "BASE", "java", "jvm ", "../base", "unknown"} {
		if Valid(bad) {
			t.Errorf("Valid(%q) = true, want false", bad)
		}
	}
}

func TestDefaultIsValidAndFirst(t *testing.T) {
	if !Valid(DefaultName) {
		t.Fatalf("DefaultName %q must be a valid template", DefaultName)
	}
	if len(Names) == 0 || Names[0] != DefaultName {
		t.Fatalf("Names[0] = %q, want DefaultName %q first (display order)", Names, DefaultName)
	}
}

func TestWellFormed(t *testing.T) {
	// Registry names and unknown-but-well-formed names both pass (an unknown name
	// is the drift signal, not an error).
	for _, ok := range []string{"base", "jvm", "kubectl-heavy", "a", "go-1-22"} {
		if !WellFormed(ok) {
			t.Errorf("WellFormed(%q) = false, want true", ok)
		}
	}
	// Untrusted junk a hostile worker might send is rejected before the DB/UI.
	bad := []string{
		"", " ", "base ", "BASE", "jvm/../etc", "../base", "a/b",
		"has space", "dot.name", "under_score", "emoji😀",
	}
	for _, b := range bad {
		if WellFormed(b) {
			t.Errorf("WellFormed(%q) = true, want false", b)
		}
	}
	// Over the 64-char bound.
	long := ""
	for i := 0; i < 65; i++ {
		long += "a"
	}
	if WellFormed(long) {
		t.Errorf("WellFormed(65 chars) = true, want false")
	}
}
