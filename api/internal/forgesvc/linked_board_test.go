package forgesvc

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/issuesource"
	"github.com/vtmocanu/uzi/api/internal/issuetracker"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #4173 M5: the linked board. The guard lets the move and the State write through,
// the cache keeps a linked card's column in issue_tracker_column, and a refused move
// changes neither the issue tracker nor the cache.

// movingDriver is the inner driver of a linked guard: it records the moves and State
// writes that reach it and answers them with err.
type movingDriver struct {
	forge.IssueTracker // nil: only what a test drives exists
	moves              []forge.ColumnMove
	states             []forge.IssueState
	err                error
	// The create and label writes PRD #4173 M5 lifts through the guard.
	creates  [][]string
	ensured  [][]forge.Label
	labelOps [][2][]string
	// The description and note writes PRD #4173 M5 U6 lifts through the guard.
	descriptions []string
	notes        []string
}

func (d *movingDriver) UpdateIssueDescription(_ context.Context, _, _ int64, desc string) error {
	d.descriptions = append(d.descriptions, desc)
	return d.err
}

func (d *movingDriver) CreateIssueNote(_ context.Context, _, _ int64, body string) (forge.IssueNote, error) {
	d.notes = append(d.notes, body)
	return forge.IssueNote{}, d.err
}

func (d *movingDriver) CreateIssue(_ context.Context, _ int64, _, _ string, labels []string) (forge.Issue, error) {
	d.creates = append(d.creates, labels)
	return forge.Issue{IID: 5}, d.err
}

func (d *movingDriver) EnsureLabels(_ context.Context, _ int64, labels []forge.Label) error {
	d.ensured = append(d.ensured, labels)
	return d.err
}

func (d *movingDriver) UpdateIssueLabels(_ context.Context, _, _ int64, add, remove []string) error {
	d.labelOps = append(d.labelOps, [2][]string{add, remove})
	return d.err
}

func (d *movingDriver) MoveToColumn(_ context.Context, _, _ int64, m forge.ColumnMove) error {
	d.moves = append(d.moves, m)
	return d.err
}

func (d *movingDriver) SetIssueState(_ context.Context, _, _ int64, s forge.IssueState) error {
	d.states = append(d.states, s)
	return d.err
}

func TestLinkedIssueResolvedNamesOnlyTheResolvedRefusal(t *testing.T) {
	if !LinkedIssueResolved(fmt.Errorf("move: %w", issuetracker.ErrIssueResolved)) {
		t.Error("a wrapped ErrIssueResolved must be reported")
	}
	if !LinkedIssueResolved((&issueWriteGuard{inner: &movingDriver{err: issuetracker.ErrIssueResolved}}).MoveToColumn(context.Background(), 1, 2, forge.ColumnMove{})) {
		t.Error("the write guard must keep ErrIssueResolved in the chain")
	}
	for _, e := range []error{nil, issuetracker.ErrColumnNotMapped, ErrIssueWriteNotOffered, errors.New("503")} {
		if LinkedIssueResolved(e) {
			t.Errorf("LinkedIssueResolved(%v) = true, want false", e)
		}
	}
	// Neither a skip nor a refusal: each has its own caller handling.
	if IssueWriteSkippable(issuetracker.ErrIssueResolved) || ColumnMoveRefused(issuetracker.ErrIssueResolved) {
		t.Error("ErrIssueResolved must be neither IssueWriteSkippable nor ColumnMoveRefused")
	}
}

func TestColumnMoveRefusedNamesOnlyTheDriversRefusals(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"column not mapped", issuetracker.ErrColumnNotMapped, true},
		{"mapping unset", issuetracker.ErrStateMappingUnset, true},
		{"state not allowed", issuetracker.ErrStateNotAllowed, true},
		{"wrapped", fmt.Errorf("move: %w", issuetracker.ErrColumnNotMapped), true},
		{"write not offered is not a refusal", ErrIssueWriteNotOffered, false},
		{"moved away is not a refusal", &linkedIssueError{err: &forge.IssueNotFoundError{Err: errors.New("404")}}, false},
		{"other error", errors.New("youtrack unreachable"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ColumnMoveRefused(tc.err); got != tc.want {
				t.Fatalf("ColumnMoveRefused(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
	// IssueWriteSkippable is NOT widened: a refusal keeps its retry marker.
	for _, e := range []error{issuetracker.ErrColumnNotMapped, issuetracker.ErrStateMappingUnset, issuetracker.ErrStateNotAllowed} {
		if IssueWriteSkippable(e) {
			t.Errorf("IssueWriteSkippable(%v) = true, want false", e)
		}
	}
}

func TestGuardPassesMovesAndStateWritesThroughAndMarksNotFound(t *testing.T) {
	ctx := context.Background()
	moved := forge.ColumnMove{Column: "In Progress"}

	t.Run("reach the driver", func(t *testing.T) {
		d := &movingDriver{}
		g := &issueWriteGuard{inner: d}
		if err := g.MoveToColumn(ctx, 1, 2, moved); err != nil {
			t.Fatal(err)
		}
		if err := g.SetIssueState(ctx, 1, 2, forge.StateClosed); err != nil {
			t.Fatal(err)
		}
		if len(d.moves) != 1 || d.moves[0].Column != "In Progress" || len(d.states) != 1 || d.states[0] != forge.StateClosed {
			t.Fatalf("driver saw moves=%v states=%v", d.moves, d.states)
		}
	})

	t.Run("a moved-away issue is a skippable linked error", func(t *testing.T) {
		d := &movingDriver{err: &forge.IssueNotFoundError{Err: errors.New("404")}}
		g := &issueWriteGuard{inner: d}
		for name, err := range map[string]error{
			"move":  g.MoveToColumn(ctx, 1, 2, moved),
			"state": g.SetIssueState(ctx, 1, 2, forge.StateClosed),
		} {
			if !LinkedIssueGone(err) || !IssueWriteSkippable(err) {
				t.Errorf("%s: %v must be a marked, skippable not-found", name, err)
			}
			var missing *forge.IssueNotFoundError
			if !errors.As(err, &missing) {
				t.Errorf("%s: the marked error must still unwrap to the not-found", name)
			}
		}
	})

	t.Run("refusals and other errors pass unmarked", func(t *testing.T) {
		for _, cause := range []error{issuetracker.ErrColumnNotMapped, issuetracker.ErrStateNotAllowed, errors.New("503")} {
			g := &issueWriteGuard{inner: &movingDriver{err: cause}}
			for _, err := range []error{g.MoveToColumn(ctx, 1, 2, moved), g.SetIssueState(ctx, 1, 2, forge.StateOpened)} {
				if !errors.Is(err, cause) || IssueWriteSkippable(err) {
					t.Errorf("error %v for cause %v must keep the cause and not be skippable", err, cause)
				}
			}
		}
	})

	// Converted from "every other write is still refused without reaching the driver": create,
	// ensure and label writes are lifted by PRD #4173 M5; the description and note writes are
	// lifted by U6.
	t.Run("create and label writes reach the driver", func(t *testing.T) {
		d := &movingDriver{}
		g := &issueWriteGuard{inner: d, eligibility: "ready"}
		if err := g.EnsureLabels(ctx, 1, []forge.Label{{Name: "x"}}); err != nil {
			t.Fatalf("ensure: %v", err)
		}
		if iss, err := g.CreateIssue(ctx, 1, "t", "d", []string{"ready"}); err != nil || iss.IID != 5 {
			t.Fatalf("create: %v %v", iss, err)
		}
		if err := g.UpdateIssueLabels(ctx, 1, 2, []string{"ready"}, []string{"other"}); err != nil {
			t.Fatalf("labels: %v", err)
		}
		if len(d.ensured) != 1 || len(d.creates) != 1 || len(d.labelOps) != 1 {
			t.Fatalf("driver saw ensure=%d create=%d labels=%d, want 1 each", len(d.ensured), len(d.creates), len(d.labelOps))
		}
	})

	t.Run("a moved-away issue on a label write is a skippable linked error", func(t *testing.T) {
		g := &issueWriteGuard{inner: &movingDriver{err: &forge.IssueNotFoundError{Err: errors.New("404")}}, eligibility: "ready"}
		err := g.UpdateIssueLabels(ctx, 1, 2, []string{"a"}, nil)
		if !LinkedIssueGone(err) || !IssueWriteSkippable(err) {
			t.Fatalf("%v must be a marked, skippable not-found", err)
		}
	})

	t.Run("removing the eligibility name is refused without reaching the driver", func(t *testing.T) {
		d := &movingDriver{}
		g := &issueWriteGuard{inner: d, eligibility: "ready"}
		err := g.UpdateIssueLabels(ctx, 1, 2, nil, []string{"other", "ready"})
		if !errors.Is(err, ErrIssueWriteNotOffered) || !IssueWriteSkippable(err) {
			t.Fatalf("err = %v, want the skippable ErrIssueWriteNotOffered", err)
		}
		if len(d.labelOps) != 0 {
			t.Fatalf("the driver saw %v", d.labelOps)
		}
		// Adding it, or removing another label, is not a removal of the eligibility name.
		if err := g.UpdateIssueLabels(ctx, 1, 2, []string{"ready"}, []string{"other"}); err != nil {
			t.Fatalf("add eligibility: %v", err)
		}
		// The comparison is exact: a different case or a longer name is not the eligibility name.
		d.labelOps = nil
		if err := g.UpdateIssueLabels(ctx, 1, 2, nil, []string{"Ready", "ready-x", "other"}); err != nil {
			t.Fatalf("near-miss removal: %v", err)
		}
		if len(d.labelOps) != 1 || len(d.labelOps[0][1]) != 3 {
			t.Fatalf("the driver saw %v, want the near-miss removal passed through", d.labelOps)
		}
	})

	t.Run("a moved-away issue on create and ensure is a marked not-found", func(t *testing.T) {
		g := &issueWriteGuard{inner: &movingDriver{err: &forge.IssueNotFoundError{Err: errors.New("404")}}, eligibility: "ready"}
		_, createErr := g.CreateIssue(ctx, 1, "t", "d", []string{"ready"})
		for name, err := range map[string]error{
			"create": createErr,
			"ensure": g.EnsureLabels(ctx, 1, []forge.Label{{Name: "x"}}),
		} {
			if !LinkedIssueGone(err) || !IssueWriteSkippable(err) {
				t.Errorf("%s: %v must be a marked, skippable not-found", name, err)
			}
		}
	})

	// Converted from "description and note writes are still refused without reaching the
	// driver": PRD #4173 M5 U6 lifts both, so they reach the driver.
	t.Run("description and note writes reach the driver", func(t *testing.T) {
		d := &movingDriver{}
		g := &issueWriteGuard{inner: d}
		if err := g.UpdateIssueDescription(ctx, 1, 2, "d"); err != nil {
			t.Errorf("desc: %v", err)
		}
		if _, err := g.CreateIssueNote(ctx, 1, 2, "n"); err != nil {
			t.Errorf("note: %v", err)
		}
		if len(d.descriptions) != 1 || d.descriptions[0] != "d" || len(d.notes) != 1 || d.notes[0] != "n" {
			t.Fatalf("driver saw descriptions=%v notes=%v", d.descriptions, d.notes)
		}
	})

	t.Run("a moved-away issue on a note and a description is a skippable linked error", func(t *testing.T) {
		g := &issueWriteGuard{inner: &movingDriver{err: &forge.IssueNotFoundError{Err: errors.New("404")}}}
		_, noteErr := g.CreateIssueNote(ctx, 1, 2, "n")
		for name, err := range map[string]error{
			"note": noteErr,
			"desc": g.UpdateIssueDescription(ctx, 1, 2, "d"),
		} {
			if !LinkedIssueGone(err) || !IssueWriteSkippable(err) || errors.Is(err, ErrIssueWriteNotOffered) {
				t.Errorf("%s: %v must be a marked, skippable not-found", name, err)
			}
		}
	})

	t.Run("an unavailable driver's note and description writes return its cause", func(t *testing.T) {
		cause := errors.New("connection removed")
		g := UnavailableIssueTracker(cause)
		if _, err := g.CreateIssueNote(ctx, 1, 2, "n"); !errors.Is(err, cause) || IssueWriteSkippable(err) {
			t.Errorf("note: %v", err)
		}
		if err := g.UpdateIssueDescription(ctx, 1, 2, "d"); !errors.Is(err, cause) || IssueWriteSkippable(err) {
			t.Errorf("desc: %v", err)
		}
	})

	t.Run("an unavailable driver's create and label writes return its cause", func(t *testing.T) {
		cause := errors.New("connection removed")
		g := UnavailableIssueTracker(cause)
		if _, err := g.CreateIssue(ctx, 1, "t", "d", nil); !errors.Is(err, cause) || IssueWriteSkippable(err) {
			t.Errorf("create: %v", err)
		}
		if err := g.EnsureLabels(ctx, 1, nil); !errors.Is(err, cause) || IssueWriteSkippable(err) {
			t.Errorf("ensure: %v", err)
		}
		if err := g.UpdateIssueLabels(ctx, 1, 2, []string{"a"}, nil); !errors.Is(err, cause) || IssueWriteSkippable(err) {
			t.Errorf("labels: %v", err)
		}
	})
}

type statedInner struct {
	movingDriver
	got  forge.ListIssuesOptions
	list []issuesource.StatedIssue
	err  error
}

func (s *statedInner) ListIssuesWithState(_ context.Context, _ int64, opts forge.ListIssuesOptions) ([]issuesource.StatedIssue, error) {
	s.got = opts
	return s.list, s.err
}

func TestGuardListIssuesWithState(t *testing.T) {
	ctx := context.Background()
	opts := forge.ListIssuesOptions{Labels: []string{"uzi"}}

	t.Run("forwards to a driver that offers it", func(t *testing.T) {
		in := &statedInner{list: []issuesource.StatedIssue{{Issue: forge.Issue{IID: 5}, State: "In Progress"}}}
		got, err := (&issueWriteGuard{inner: in}).ListIssuesWithState(ctx, 1, opts)
		if err != nil || len(got) != 1 || got[0].State != "In Progress" || len(in.got.Labels) != 1 {
			t.Fatalf("got (%v, %v) with options %+v", got, err, in.got)
		}
	})

	t.Run("an inner failure is returned", func(t *testing.T) {
		boom := errors.New("boom")
		got, err := (&issueWriteGuard{inner: &statedInner{err: boom}}).ListIssuesWithState(ctx, 1, opts)
		if !errors.Is(err, boom) || got != nil {
			t.Fatalf("got (%v, %v), want the cause", got, err)
		}
	})

	t.Run("a driver without it is an error, never an empty listing", func(t *testing.T) {
		got, err := (&issueWriteGuard{inner: &movingDriver{}}).ListIssuesWithState(ctx, 1, opts)
		if err == nil || !errors.Is(err, errNoStateListing) || got != nil {
			t.Fatalf("got (%v, %v), want an error and no listing", got, err)
		}
	})

	t.Run("the unavailable issue tracker returns its cause", func(t *testing.T) {
		cause := errors.New("issue tracker unavailable")
		tr := UnavailableIssueTracker(cause)
		lister, ok := tr.(issuesource.IssueStateLister)
		if !ok {
			t.Fatal("the linked guard must offer ListIssuesWithState")
		}
		got, err := lister.ListIssuesWithState(ctx, 1, opts)
		if !errors.Is(err, cause) || got != nil || IssueWriteSkippable(err) {
			t.Fatalf("got (%v, %v), want the cause, no listing, not skippable", got, err)
		}
		// Moves and State writes of an unavailable side fail with the cause, so a
		// resolver outage is retried, never skipped.
		if err := tr.MoveToColumn(ctx, 1, 2, forge.ColumnMove{Column: "x"}); !errors.Is(err, cause) || IssueWriteSkippable(err) {
			t.Fatalf("move error = %v, want the cause, not skippable", err)
		}
		if err := tr.SetIssueState(ctx, 1, 2, forge.StateClosed); !errors.Is(err, cause) || IssueWriteSkippable(err) {
			t.Fatalf("state error = %v, want the cause, not skippable", err)
		}
	})
}

// ── AutoMove, close and reopen on a linked repo ───────────────────────────────

func linkedIssue(state, column string) store.Issue {
	iss := stateIssue2(state)
	if column != "" {
		iss.IssueTrackerColumn.String, iss.IssueTrackerColumn.Valid = column, true
	}
	return iss
}

func stateIssue2(state string) store.Issue {
	return store.Issue{RepoID: uuid.New(), ForgeIssueIid: 42, Title: "t", State: state, Labels: []byte(`["uzi","Planned"]`)}
}

func TestAutoMoveOnALinkedRepoCachesOnlyTheColumn(t *testing.T) {
	st := &fakeStore{}
	d := &movingDriver{}
	cols := []store.BoardColumn{{LabelName: "In Progress", Position: 0}}

	got, err := newTestService(st).AutoMove(context.Background(), &issueWriteGuard{inner: d}, 7, linkedIssue("opened", "Planned"), cols, "In Progress")
	if err != nil {
		t.Fatal(err)
	}
	if len(d.moves) != 1 || d.moves[0].Column != "In Progress" {
		t.Fatalf("driver moves = %v", d.moves)
	}
	if len(st.labelUpserts) != 0 {
		t.Fatalf("a linked move wrote labels: %+v", st.labelUpserts)
	}
	if len(st.columnWrites) != 1 || st.columnWrites[0].IssueTrackerColumn != "In Progress" || st.columnWrites[0].ForgeIssueIid != 42 {
		t.Fatalf("column writes = %+v, want In Progress for issue 42", st.columnWrites)
	}
	if got.IssueTrackerColumn.String != "In Progress" {
		t.Fatalf("returned column = %+v", got.IssueTrackerColumn)
	}
}

func TestAutoMoveOnALinkedRepoLeavesTheCacheOnAFailure(t *testing.T) {
	for name, cause := range map[string]error{
		"refused":     issuetracker.ErrColumnNotMapped,
		"unreachable": errors.New("503"),
		"moved away":  &forge.IssueNotFoundError{Err: errors.New("404")},
	} {
		t.Run(name, func(t *testing.T) {
			st := &fakeStore{}
			_, err := newTestService(st).AutoMove(context.Background(), &issueWriteGuard{inner: &movingDriver{err: cause}}, 7, linkedIssue("opened", ""), nil, "In Progress")
			if !errors.Is(err, cause) {
				t.Fatalf("error = %v, want the cause", err)
			}
			if len(st.columnWrites) != 0 || len(st.labelUpserts) != 0 {
				t.Fatalf("a failed move changed the cache: columns=%v labels=%v", st.columnWrites, st.labelUpserts)
			}
		})
	}
}

// A move to the Open lane stores no column (the Open lane is NULL).
func TestAutoMoveOnALinkedRepoToOpenWritesAnEmptyColumn(t *testing.T) {
	st := &fakeStore{}
	got, err := newTestService(st).AutoMove(context.Background(), &issueWriteGuard{inner: &movingDriver{}}, 7, linkedIssue("opened", "Planned"), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(st.columnWrites) != 1 || st.columnWrites[0].IssueTrackerColumn != "" || got.IssueTrackerColumn.Valid {
		t.Fatalf("column writes = %+v, returned %+v", st.columnWrites, got.IssueTrackerColumn)
	}
}

func TestCloseIssueOnALinkedRepoWritesTheClosedState(t *testing.T) {
	st := &fakeStore{}
	d := &movingDriver{}
	if _, err := newTestService(st).CloseIssue(context.Background(), &issueWriteGuard{inner: d}, 7, linkedIssue("opened", "Planned")); err != nil {
		t.Fatal(err)
	}
	if len(d.states) != 1 || d.states[0] != forge.StateClosed || len(st.stateUpdates) != 1 {
		t.Fatalf("states=%v stateUpdates=%v", d.states, st.stateUpdates)
	}
	if len(st.columnWrites) != 0 {
		t.Fatalf("close must not touch the column: %v", st.columnWrites)
	}

	// A refused close (no closed State saved) changes nothing.
	st = &fakeStore{}
	_, err := newTestService(st).CloseIssue(context.Background(), &issueWriteGuard{inner: &movingDriver{err: issuetracker.ErrStateMappingUnset}}, 7, linkedIssue("opened", ""))
	if !errors.Is(err, issuetracker.ErrStateMappingUnset) || len(st.stateUpdates) != 0 {
		t.Fatalf("err=%v stateUpdates=%v", err, st.stateUpdates)
	}
}

func TestReopenIssueOnALinkedRepoIsOneMoveThenTheCache(t *testing.T) {
	t.Run("one write, then the cache", func(t *testing.T) {
		st := &fakeStore{}
		d := &movingDriver{}
		got, err := newTestService(st).ReopenIssue(context.Background(), &issueWriteGuard{inner: d}, 7, linkedIssue("closed", ""), nil, "In Progress")
		if err != nil {
			t.Fatal(err)
		}
		if len(d.states) != 0 {
			t.Fatalf("a linked reopen must not write the State separately: %v", d.states)
		}
		if len(d.moves) != 1 || d.moves[0].Column != "In Progress" {
			t.Fatalf("moves = %v, want one move to In Progress", d.moves)
		}
		if !d.moves[0].Reopen {
			t.Fatalf("move = %+v, want Reopen set so the driver writes over a resolved issue", d.moves[0])
		}
		if len(st.reopens) != 1 || len(st.columnWrites) != 1 || st.columnWrites[0].IssueTrackerColumn != "In Progress" {
			t.Fatalf("reopens=%v columnWrites=%v", st.reopens, st.columnWrites)
		}
		if got.IssueTrackerColumn.String != "In Progress" {
			t.Fatalf("returned column=%+v", got.IssueTrackerColumn)
		}
		if len(st.labelUpserts) != 0 {
			t.Fatalf("linked reopen wrote labels: %v", st.labelUpserts)
		}
	})

	t.Run("a refused or failed move changes neither side", func(t *testing.T) {
		for name, cause := range map[string]error{
			"unmapped column": issuetracker.ErrColumnNotMapped,
			"not allowed":     issuetracker.ErrStateNotAllowed,
			"unreachable":     errors.New("503"),
		} {
			st := &fakeStore{}
			d := &movingDriver{err: cause}
			_, err := newTestService(st).ReopenIssue(context.Background(), &issueWriteGuard{inner: d}, 7, linkedIssue("closed", ""), nil, "Nowhere")
			if !errors.Is(err, cause) {
				t.Fatalf("%s: error = %v", name, err)
			}
			if len(d.states) != 0 || len(st.reopens) != 0 || len(st.columnWrites) != 0 {
				t.Fatalf("%s: states=%v reopens=%v columns=%v", name, d.states, st.reopens, st.columnWrites)
			}
		}
	})
}

// ── the linked sync ───────────────────────────────────────────────────────────

func TestLinkedSyncCachesTheColumnOfEachStatesMapping(t *testing.T) {
	user, repo := uuid.New(), uuid.New()
	closed := issueAt(9, recordedMarkBase)
	closed.State = "closed"
	f := &fakeForge{
		issues: []forge.Issue{issueAt(7, recordedMarkBase), issueAt(8, recordedMarkBase), closed, issueAt(10, recordedMarkBase), issueAt(11, recordedMarkBase), issueAt(12, recordedMarkBase)},
		states: map[int64]string{7: "In Progress", 8: "Some Custom State", 9: "In Progress", 10: "Open", 11: "", 12: "In Review"},
	}
	st := newLinkedSyncStore()
	st.row.ColumnStates = []byte(`{"In Progress":"In Progress","Human Review":"In Review"}`)

	if _, err := newTestService(st).SyncIssues(context.Background(), user, repo, testProjectID, linkedTracker(f)); err != nil {
		t.Fatal(err)
	}
	want := map[int64]string{7: "In Progress", 8: "", 9: "", 10: "", 11: "", 12: "Human Review"}
	if len(st.upserts) != len(want) {
		t.Fatalf("upserts = %d", len(st.upserts))
	}
	for i, up := range st.upserts {
		col := st.changedColumns[i]
		if col.String != want[up.ForgeIssueIid] || col.Valid != (want[up.ForgeIssueIid] != "") {
			t.Errorf("issue %d cached column %+v, want %q (NULL for the Open lane)", up.ForgeIssueIid, col, want[up.ForgeIssueIid])
		}
	}
}

// The forge sync never writes the column.
func TestForgeSyncDoesNotWriteTheColumn(t *testing.T) {
	st := &fakeStore{}
	f := &fakeForge{issues: []forge.Issue{issueAt(1, time.Unix(100, 0))}, states: map[int64]string{1: "In Progress"}}
	if _, err := newTestService(st).FullSync(context.Background(), uuid.New(), testProjectID, f); err != nil {
		t.Fatal(err)
	}
	if st.changedUpserts != 0 || len(st.columnWrites) != 0 {
		t.Fatalf("the forge sync touched the column path: changed=%d columns=%v", st.changedUpserts, st.columnWrites)
	}
}

// A linked side that cannot list States fails the sync with no write and no eviction.
func TestLinkedSyncFailsWhenTheDriverCannotListStates(t *testing.T) {
	st := newLinkedSyncStore()
	_, err := newTestService(st).SyncIssues(context.Background(), uuid.New(), uuid.New(), testProjectID, &issueWriteGuard{inner: &movingDriver{}})
	if !errors.Is(err, errNoStateListing) {
		t.Fatalf("error = %v, want errNoStateListing", err)
	}
	if len(st.upserts) != 0 || len(st.deleteCalls) != 0 {
		t.Fatalf("a failed listing wrote or evicted: upserts=%d deletes=%v", len(st.upserts), st.deleteCalls)
	}
}
