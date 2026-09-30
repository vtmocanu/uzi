package skilltmpl

import (
	"reflect"
	"testing"
)

// TestScopesVocabulary pins the scope vocabulary's values and order (the skills_scope_check CHECK
// and workersvc.scopeRank are pinned to it by their own tests) and the shared-scope predicate.
func TestScopesVocabulary(t *testing.T) {
	want := []string{"builtin", "global", "user", "product"}
	if got := Scopes(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Scopes() = %v, want %v", got, want)
	}
	got := Scopes()
	got[0] = "mutated"
	if Scopes()[0] != "builtin" {
		t.Error("Scopes() returned its backing slice")
	}
	for scope, shared := range map[string]bool{"builtin": true, "global": true, "user": false, "product": false, "": false, "other": false} {
		if IsShared(scope) != shared {
			t.Errorf("IsShared(%q) = %v, want %v", scope, !shared, shared)
		}
	}
}
