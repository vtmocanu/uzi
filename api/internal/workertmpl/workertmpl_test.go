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
