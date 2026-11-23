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

// moveInner stubs the label writes and fails the test if the Recorder ever
// forwards MoveToColumn to it.
type moveInner struct {
	BaseFake
	t          *testing.T
	underlying []string
}

func (m *moveInner) EnsureLabels(context.Context, int64, []forge.Label) error {
	m.underlying = append(m.underlying, "EnsureLabels")
	return nil
}

func (m *moveInner) UpdateIssueLabels(context.Context, int64, int64, []string, []string) error {
	m.underlying = append(m.underlying, "UpdateIssueLabels")
	return nil
}

func (m *moveInner) MoveToColumn(context.Context, int64, int64, forge.ColumnMove) error {
	m.t.Error("Recorder forwarded MoveToColumn to the wrapped Forge")
	return nil
}

func TestRecorderMoveToColumnRecordsTheLabelBackedMove(t *testing.T) {
	inner := &moveInner{t: t}
	rec := NewRecorder(inner)
	move := forge.ColumnMove{
		Column: "In Progress",
		Ensure: []forge.Label{{Name: "In Progress", Color: "#123456"}},
		Add:    []string{"In Progress"},
		Remove: []string{"Planned"},
	}
	if err := rec.MoveToColumn(context.Background(), 42, 7, move); err != nil {
		t.Fatal(err)
	}

	wantMoves := []string{`MoveToColumn(42, 7, {Column:"In Progress" Ensure:[{Name:"In Progress" Color:"#123456"}] Add:["In Progress"] Remove:["Planned"] Reopen:false})`}
	if got := rec.Moves(); !slices.Equal(got, wantMoves) {
		t.Fatalf("moves = %q, want %q", got, wantMoves)
	}
	wantCalls := []string{
		`EnsureLabels(42, [{Name:"In Progress" Color:"#123456"}])`,
		`UpdateIssueLabels(42, 7, ["In Progress"], ["Planned"])`,
	}
	if got := rec.Calls(); !slices.Equal(got, wantCalls) {
		t.Fatalf("calls = %q, want %q", got, wantCalls)
	}
	if want := []string{"EnsureLabels", "UpdateIssueLabels"}; !slices.Equal(inner.underlying, want) {
		t.Fatalf("wrapped calls = %v, want %v", inner.underlying, want)
	}
}
