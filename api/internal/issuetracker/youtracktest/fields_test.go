package youtracktest

import (
	"reflect"
	"testing"
)

func TestFieldsProjection(t *testing.T) {
	spec, ok := parseFields("id,author(id,login),tags(name),$type")
	if !ok {
		t.Fatal("parse failed")
	}
	in := map[string]any{
		"id": "1-1", "extra": 1, "$type": "Issue",
		"author": map[string]any{"id": "2", "login": "a", "x": 1},
		"tags":   []map[string]any{{"name": "n", "id": "9"}},
	}
	want := map[string]any{
		"id": "1-1", "$type": "Issue",
		"author": map[string]any{"id": "2", "login": "a"},
		"tags":   []any{map[string]any{"name": "n"}},
	}
	if got := spec.project(in); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v", got)
	}
	for _, bad := range []string{"", "a(", "a,,b", "a(b", "(a)"} {
		if _, ok := parseFields(bad); ok {
			t.Errorf("parseFields(%q) accepted", bad)
		}
	}
}
