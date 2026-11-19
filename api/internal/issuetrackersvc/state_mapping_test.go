package issuetrackersvc

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/issuetracker/youtracktest"
)

// linkedMappingRepo links a fresh repo to the fake project, gives it the board columns
// and seeds the States the mapping tests use.
func linkedMappingRepo(t *testing.T, h *ytHarness) uuid.UUID {
	t.Helper()
	repo := h.q.addRepo(h.user, true)
	if err := h.link(repo, youtracktest.EligibilityTagID); err != nil {
		t.Fatal(err)
	}
	h.q.setBoardColumns(repo, "In Progress", "Review")
	h.srv.SeedState("Doing", false)
	h.srv.SeedState("In Review", false)
	h.srv.SeedState("Shipped", true)
	return repo
}

func wantMappingErr(t *testing.T, err error, kind Kind, substr string) {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("err = %v, want a typed *Error", err)
	}
	if e.Kind != kind || !strings.Contains(e.Message, substr) {
		t.Fatalf("err = {%v %q}, want kind %v containing %q", e.Kind, e.Message, kind, substr)
	}
}

func validMapping() StateMapping {
	return StateMapping{
		OpenState: "Open", ClosedState: "Fixed",
		Columns: []ColumnState{{LabelName: "In Progress", State: "Doing"}, {LabelName: "Review", State: "In Review"}},
	}
}

func TestSetStateMappingSavesAndReadsBack(t *testing.T) {
	h := newYTHarness(t)
	repo := linkedMappingRepo(t, h)
	ctx := context.Background()

	view, err := h.svc.GetStateMapping(ctx, h.user, repo)
	if err != nil {
		t.Fatal(err)
	}
	if view.Mapping != nil {
		t.Fatalf("mapping before any save = %+v, want nil", view.Mapping)
	}
	byName := map[string]StateInfo{}
	for _, s := range view.States {
		byName[s.Name] = s
	}
	if s := byName["Fixed"]; !s.Closable || s.Openable {
		t.Errorf("Fixed = %+v, want closable and not openable", s)
	}
	if s := byName["Won't fix"]; s.Closable || s.Openable || !s.Resolved {
		t.Errorf("Won't fix = %+v, want resolved, neither closable nor openable", s)
	}
	if s := byName["Open"]; s.Closable || !s.Openable {
		t.Errorf("Open = %+v, want openable and not closable", s)
	}

	if _, err := h.svc.SetStateMapping(ctx, h.user, repo, validMapping()); err != nil {
		t.Fatal(err)
	}
	view, err = h.svc.GetStateMapping(ctx, h.user, repo)
	if err != nil {
		t.Fatal(err)
	}
	m := view.Mapping
	if m == nil || m.OpenState != "Open" || m.ClosedState != "Fixed" || len(m.Columns) != 2 ||
		m.Columns[0] != (ColumnState{"In Progress", "Doing"}) || m.Columns[1] != (ColumnState{"Review", "In Review"}) {
		t.Fatalf("mapping = %+v", m)
	}

	// A second save replaces the whole mapping, columns included.
	next := StateMapping{OpenState: "Doing", ClosedState: "Shipped", Columns: []ColumnState{{LabelName: "Review", State: "In Review"}}}
	if _, err := h.svc.SetStateMapping(ctx, h.user, repo, next); err != nil {
		t.Fatal(err)
	}
	view, _ = h.svc.GetStateMapping(ctx, h.user, repo)
	if m := view.Mapping; m == nil || m.OpenState != "Doing" || m.ClosedState != "Shipped" || len(m.Columns) != 1 || m.Columns[0].LabelName != "Review" {
		t.Fatalf("replaced mapping = %+v", m)
	}
}

func TestSetStateMappingRefusals(t *testing.T) {
	h := newYTHarness(t)
	repo := linkedMappingRepo(t, h)
	h.srv.SeedState("Archived", false)
	h.srv.SetStateArchived("Archived", true)
	ctx := context.Background()

	edit := func(f func(*StateMapping)) StateMapping {
		m := validMapping()
		f(&m)
		return m
	}
	cases := []struct {
		name string
		in   StateMapping
		kind Kind
		msg  string
	}{
		{"unknown open state", edit(func(m *StateMapping) { m.OpenState = "Nope" }), KindBadRequest, `"Nope"`},
		{"unknown column state", edit(func(m *StateMapping) { m.Columns[0].State = "Nope" }), KindBadRequest, "not a state"},
		{"resolved column state", edit(func(m *StateMapping) { m.Columns[0].State = "Shipped" }), KindBadRequest, "column state"},
		{"resolved open state", edit(func(m *StateMapping) { m.OpenState = "Shipped" }), KindBadRequest, "open state"},
		{"won't fix closed", edit(func(m *StateMapping) { m.ClosedState = "Won't fix" }), KindBadRequest, "Won't fix"},
		{"unresolved closed", edit(func(m *StateMapping) { m.ClosedState = "Doing" }), KindBadRequest, "closed state"},
		{"archived column state", edit(func(m *StateMapping) { m.Columns[0].State = "Archived" }), KindBadRequest, "column state"},
		{"label not a column", edit(func(m *StateMapping) { m.Columns[0].LabelName = "Backlog" }), KindBadRequest, "not a board column"},
		{"open equals closed", edit(func(m *StateMapping) { m.ClosedState = "Open" }), KindBadRequest, "different"},
		{"column repeats open", edit(func(m *StateMapping) { m.Columns[0].State = "Open" }), KindBadRequest, "more than once"},
		{"two columns, one state", edit(func(m *StateMapping) { m.Columns[1].State = "Doing" }), KindBadRequest, "more than once"},
		{"duplicate label", edit(func(m *StateMapping) { m.Columns[1].LabelName = "In Progress" }), KindBadRequest, "more than once"},
		{"empty name", edit(func(m *StateMapping) { m.OpenState = "" }), KindBadRequest, "empty"},
		{"control character", edit(func(m *StateMapping) { m.Columns[0].LabelName = "a\nb" }), KindBadRequest, "control"},
		{"zero-width label", edit(func(m *StateMapping) { m.Columns[0].LabelName = "In\u200bProgress" }), KindBadRequest, "control"},
		{"bidi state", edit(func(m *StateMapping) { m.OpenState = "Op\u202een" }), KindBadRequest, "control"},
		{"filler label", edit(func(m *StateMapping) { m.Columns[0].LabelName = "Review\u3164" }), KindBadRequest, "blank-filler"},
		{"variation selector state", edit(func(m *StateMapping) { m.OpenState = "Open\uFE0F" }), KindBadRequest, "variation-selector"},
		{"too long", edit(func(m *StateMapping) { m.ClosedState = strings.Repeat("x", 201) }), KindBadRequest, "200"},
		{"too many columns", edit(func(m *StateMapping) { m.Columns = make([]ColumnState, 11) }), KindBadRequest, "too many"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := h.svc.SetStateMapping(ctx, h.user, repo, tc.in)
			wantMappingErr(t, err, tc.kind, tc.msg)
			view, _ := h.svc.GetStateMapping(ctx, h.user, repo)
			if view.Mapping != nil {
				t.Errorf("a refused save stored %+v", view.Mapping)
			}
		})
	}
	// A long name is never echoed whole.
	_, err := h.svc.SetStateMapping(ctx, h.user, repo, edit(func(m *StateMapping) { m.OpenState = strings.Repeat("y", 150) + "\u202e" }))
	var e *Error
	if !errors.As(err, &e) || strings.ContainsRune(e.Message, '\u202e') {
		t.Errorf("message %v must not carry the raw bidi character", err)
	}
}

func TestStateMappingScopeAndLink(t *testing.T) {
	h := newYTHarness(t)
	repo := linkedMappingRepo(t, h)
	unlinked := h.q.addRepo(h.user, true)
	ctx := context.Background()

	_, err := h.svc.SetStateMapping(ctx, uuid.New(), repo, validMapping())
	wantMappingErr(t, err, KindNotFound, "repo not found")
	_, err = h.svc.GetStateMapping(ctx, uuid.New(), repo)
	wantMappingErr(t, err, KindNotFound, "repo not found")
	_, err = h.svc.SetStateMapping(ctx, h.user, unlinked, validMapping())
	wantMappingErr(t, err, KindConflict, "not linked")
	_, err = h.svc.GetStateMapping(ctx, h.user, unlinked)
	wantMappingErr(t, err, KindConflict, "not linked")
}

// A brace is refused in a column label (it is a tag-name rule) but not in a State name:
// a State is written by id and never goes into a query, and ListStates lists it.
func TestSetStateMappingAcceptsABracedState(t *testing.T) {
	h := newYTHarness(t)
	repo := linkedMappingRepo(t, h)
	h.srv.SeedState("In {Review}", false)
	m := validMapping()
	m.Columns[1].State = "In {Review}"
	if _, err := h.svc.SetStateMapping(context.Background(), h.user, repo, m); err != nil {
		t.Fatalf("SetStateMapping with a braced State: %v", err)
	}
}

// A brace in a column label is refused even when the label is a board column (one set
// before the repo was linked): the label rule is the driver's tag-name rule.
func TestSetStateMappingRefusesABracedColumnLabel(t *testing.T) {
	h := newYTHarness(t)
	repo := linkedMappingRepo(t, h)
	h.q.setBoardColumns(repo, "In {Progress}", "Review")
	m := validMapping()
	m.Columns[0].LabelName = "In {Progress}"
	_, err := h.svc.SetStateMapping(context.Background(), h.user, repo, m)
	wantMappingErr(t, err, KindBadRequest, "brace")
}
