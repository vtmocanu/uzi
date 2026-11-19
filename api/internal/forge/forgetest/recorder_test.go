package forgetest

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/forge"
)

func TestRecorderFormatsArgumentsDeterministically(t *testing.T) {
	rec := NewRecorder(&BaseFake{})
	ctx := context.Background()
	after := time.Date(2026, 3, 4, 5, 6, 7, 0, time.FixedZone("x", 3600)) // 04:06:07 UTC

	_, _ = rec.ListIssues(ctx, 42, forge.ListIssuesOptions{Labels: []string{"uzi"}, UpdatedAfter: &after})
	_, _ = rec.ListIssues(ctx, 42, forge.ListIssuesOptions{Labels: []string{}})
	_ = rec.UpdateIssueLabels(ctx, 42, 7, []string{"a"}, nil)

	want := []string{
		`ListIssues(42, {Labels:["uzi"] UpdatedAfter:2026-03-04T04:06:07Z State:"" Limit:0})`,
		`ListIssues(42, {Labels:[] UpdatedAfter:<nil> State:"" Limit:0})`,
		`UpdateIssueLabels(42, 7, ["a"], nil)`,
	}
	if got := rec.Calls(); !slices.Equal(got, want) {
		t.Fatalf("calls = %q, want %q", got, want)
	}
}

func TestRecorderFormatMapAndUnformattable(t *testing.T) {
	type hidden struct{ n int }
	if got, want := formatArg(reflectValue(hidden{3})), "{n:<unformattable int>}"; got != want {
		t.Fatalf("unexported field = %s, want %s", got, want)
	}
	if got := formatArg(reflectValue(func() {})); got != "<unformattable func()>" {
		t.Fatalf("func = %s", got)
	}
	m := map[string]int{"b": 2, "a": 1}
	for range 20 {
		if got := formatArg(reflectValue(m)); got != `map["a":1 "b":2]` {
			t.Fatalf("map format = %s", got)
		}
	}
}

func TestAssertGoldenWritesThenMatches(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv(UpdateGoldenEnv, "1")
	AssertGolden(t, "x.golden", []string{"A()", "B(1)"})
	t.Setenv(UpdateGoldenEnv, "")
	AssertGolden(t, "x.golden", []string{"A()", "B(1)"})
	b, err := os.ReadFile(filepath.Join(dir, "testdata", "x.golden")) //nolint:gosec // G304: the test temp dir
	if err != nil || string(b) != "A()\nB(1)\n" {
		t.Fatalf("golden = %q, %v", b, err)
	}
}

func reflectValue(v any) reflect.Value { return reflect.ValueOf(v) }
