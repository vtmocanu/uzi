package forgesvc

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/board"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestIssueWriteSkippableClassifier(t *testing.T) {
	notFound := &forge.IssueNotFoundError{Err: errors.New("404")}
	linked := &issueWriteGuard{inner: &stubDriverNotFound{err: notFound}}
	_, markedErr := linked.GetIssue(context.Background(), 1, 2)

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"write not offered", ErrIssueWriteNotOffered, true},
		{"write not offered, wrapped", fmt.Errorf("move: %w", ErrIssueWriteNotOffered), true},
		{"linked not found", markedErr, true},
		{"linked not found, wrapped", fmt.Errorf("patch: %w", markedErr), true},
		{"plain forge not found", notFound, false},
		{"plain forge not found, wrapped", fmt.Errorf("x: %w", notFound), false},
		{"other error", errors.New("boom"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IssueWriteSkippable(tc.err); got != tc.want {
				t.Fatalf("IssueWriteSkippable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
	var asNotFound *forge.IssueNotFoundError
	if !errors.As(markedErr, &asNotFound) {
		t.Fatal("the marked error must still unwrap to the forge not-found")
	}
}

// stubDriverNotFound answers every issue read with err.
type stubDriverNotFound struct {
	fakeForge
	err error
}

func (s *stubDriverNotFound) GetIssue(context.Context, int64, int64) (forge.Issue, error) {
	return forge.Issue{}, s.err
}
func (s *stubDriverNotFound) ListIssueComments(context.Context, int64, int64) ([]forge.IssueComment, error) {
	return nil, s.err
}
func (s *stubDriverNotFound) ListIssueLabelEvents(context.Context, int64, int64) ([]forge.LabelEvent, error) {
	return nil, s.err
}

func TestGuardMarksOnlyNotFoundReads(t *testing.T) {
	g := &issueWriteGuard{inner: &stubDriverNotFound{err: errors.New("503")}}
	if _, err := g.GetIssue(context.Background(), 1, 2); IssueWriteSkippable(err) {
		t.Fatalf("a transient read failure must not be skippable: %v", err)
	}
	g = &issueWriteGuard{inner: &stubDriverNotFound{err: &forge.IssueNotFoundError{Err: errors.New("404")}}}
	if _, err := g.ListIssueComments(context.Background(), 1, 2); !IssueWriteSkippable(err) {
		t.Fatalf("a not-found comment read must be skippable: %v", err)
	}
	if _, err := g.ListIssueLabelEvents(context.Background(), 1, 2); !IssueWriteSkippable(err) {
		t.Fatalf("a not-found label event read must be skippable: %v", err)
	}
}

// A linked repo's card is never moved; the MR edge is still recorded (D10).
func TestGuardedMRMoveOnALinkedRepoRecordsTheMRState(t *testing.T) {
	runID, repoID := uuid.New(), uuid.New()
	st := &fakeStore{
		candidates: []store.ListMRWatchCandidatesRow{candidate(runID, 9, 13, mrTxt("opened"))},
		issue:      mrIssue(repoID, 9, "opened", "PRD", board.ColumnHumanReview),
		columns:    mrCols(),
	}
	f := &fakeForge{mr: forgeMR(13, "closed")}
	guarded := &issueWriteGuard{inner: f}

	if err := newTestService(st).SyncMRStates(context.Background(), repoID, testProjectID, f, guarded); err != nil {
		t.Fatal(err)
	}
	assertNoMove(t, f)
	assertRecorded(t, st, runID, "closed")
}

// A plain (unlinked) forge failure on the same edge still leaves the edge.
func TestGuardedMRMoveForgeFailureStillDefers(t *testing.T) {
	runID, repoID := uuid.New(), uuid.New()
	st := &fakeStore{
		candidates: []store.ListMRWatchCandidatesRow{candidate(runID, 9, 13, mrTxt("opened"))},
		issue:      mrIssue(repoID, 9, "opened", "PRD", board.ColumnHumanReview),
		columns:    mrCols(),
	}
	f := &fakeForge{mr: forgeMR(13, "closed"), updateErr: errors.New("forge down")}
	if err := newTestService(st).SyncMRStates(context.Background(), repoID, testProjectID, f, f); err != nil {
		t.Fatal(err)
	}
	assertNoRecord(t, st)
}

// The PRD link patch settles its marker on a linked repo: the description write is
// not offered, so retrying it would never end.
func TestPRDLinkPatchSettlesWhenWritesAreNotOffered(t *testing.T) {
	s, st, f, id := newPRDFixture(t, nil)
	guarded := &issueWriteGuard{inner: f}
	if err := s.SyncPRDLinkPatches(context.Background(), uuid.New(), fixProject, f, guarded); err != nil {
		t.Fatal(err)
	}
	enumerated(t, st)
	if len(f.descriptionUpdates) != 0 {
		t.Fatalf("a request reached the tracker: %+v", f.descriptionUpdates)
	}
	if len(st.prdSettled) != 1 || st.prdSettled[0] != id {
		t.Fatalf("settled = %v, want the run %v", st.prdSettled, id)
	}
}

func TestPRDLinkPatchSettlesWhenTheLinkedIssueIsGone(t *testing.T) {
	s, st, f, id := newPRDFixture(t, nil)
	gone := &stubDriverNotFound{fakeForge: *f, err: &forge.IssueNotFoundError{Err: errors.New("404")}}
	guarded := &issueWriteGuard{inner: gone}
	if err := s.SyncPRDLinkPatches(context.Background(), uuid.New(), fixProject, f, guarded); err != nil {
		t.Fatal(err)
	}
	enumerated(t, st)
	if len(st.prdSettled) != 1 || st.prdSettled[0] != id {
		t.Fatalf("settled = %v, want the run %v", st.prdSettled, id)
	}
}

// A plain forge not-found on an unlinked repo keeps the marker, as before.
func TestPRDLinkPatchKeepsTheMarkerOnAPlainForgeNotFound(t *testing.T) {
	s, st, f, _ := newPRDFixture(t, func(_ *store.ListPRDLinkPatchCandidatesRow, _ *fakeStore, f *fakeForge) {
		f.getIssueErrByIID = map[int64]error{fixIssueIID: &forge.IssueNotFoundError{Err: errors.New("404")}}
	})
	runPRDPatch(t, s, f)
	enumerated(t, st)
	if len(st.prdSettled) != 0 {
		t.Fatalf("an unlinked repo's not-found must not settle: %v", st.prdSettled)
	}
}

// An unresolvable linked repo's issue side must never be nil and must not settle any
// marker: reads fail with the cause (not skippable), writes are refused.
func TestUnavailableIssueTrackerReadsFailAndWritesAreRefused(t *testing.T) {
	cause := errors.New("tracker unavailable")
	tr := UnavailableIssueTracker(cause)
	ctx := context.Background()
	if _, err := tr.GetIssue(ctx, 1, 2); !errors.Is(err, cause) || IssueWriteSkippable(err) {
		t.Fatalf("GetIssue error = %v, want the cause and not skippable", err)
	}
	if _, err := tr.ListIssues(ctx, 1, forge.ListIssuesOptions{}); !errors.Is(err, cause) {
		t.Fatalf("ListIssues error = %v", err)
	}
	if err := tr.UpdateIssueDescription(ctx, 1, 2, "d"); !IssueWriteSkippable(err) {
		t.Fatalf("a write must be refused as not offered, got %v", err)
	}
	if !isLinkedTracker(tr) {
		t.Fatal("the unavailable tracker must dispatch as linked")
	}
	if isLinkedTracker(&fakeForge{}) {
		t.Fatal("a forge driver must not dispatch as linked")
	}
}
