package forgesvc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/board"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #4691: the watcher hands the provider merge/close time and its own observation
// instant to SetRunMRState. These tests pin the handed-over values through the fakes.

func timePtr(t time.Time) *time.Time { return &t }

func TestHistoryProviderTime(t *testing.T) {
	merged := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	closed := time.Date(2026, 3, 2, 11, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		mr   forge.MergeRequest
		want *time.Time
	}{
		{"merged uses MergedAt", forge.MergeRequest{State: forge.MRStateMerged, MergedAt: &merged, ClosedAt: &closed}, &merged},
		{"merged ignores ClosedAt", forge.MergeRequest{State: forge.MRStateMerged, ClosedAt: &closed}, nil},
		{"closed uses ClosedAt", forge.MergeRequest{State: forge.MRStateClosed, MergedAt: &merged, ClosedAt: &closed}, &closed},
		{"closed ignores MergedAt", forge.MergeRequest{State: forge.MRStateClosed, MergedAt: &merged}, nil},
		{"opened is nil", forge.MergeRequest{State: forge.MRStateOpened, MergedAt: &merged, ClosedAt: &closed}, nil},
		{"locked is nil", forge.MergeRequest{State: forge.MRStateLocked, MergedAt: &merged, ClosedAt: &closed}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := historyProviderTime(tc.mr)
			if (got == nil) != (tc.want == nil) || (got != nil && !got.Equal(*tc.want)) {
				t.Fatalf("historyProviderTime = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestProviderTimeParam(t *testing.T) {
	normal := time.Date(2026, 3, 1, 10, 0, 0, 0, time.FixedZone("x", 3600))
	cases := []struct {
		name      string
		in        *time.Time
		wantValid bool
		wantUTC   time.Time
	}{
		{"nil", nil, false, time.Time{}},
		{"zero", timePtr(time.Time{}), false, time.Time{}},
		{"pre-1970", timePtr(time.Date(1969, 12, 31, 23, 59, 59, 0, time.UTC)), false, time.Time{}},
		{"epoch is valid", timePtr(time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)), true, time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)},
		{"post-9999", timePtr(time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)), false, time.Time{}},
		{"9999 final fractional second is valid", timePtr(time.Date(9999, 12, 31, 23, 59, 59, 500_000_000, time.UTC)), true, time.Date(9999, 12, 31, 23, 59, 59, 500_000_000, time.UTC)},
		{"9999 end is valid", timePtr(time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)), true, time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)},
		{"normal converts to UTC", &normal, true, normal.UTC()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := providerTimeParam(tc.in)
			if got.Valid != tc.wantValid {
				t.Fatalf("Valid = %v, want %v", got.Valid, tc.wantValid)
			}
			if tc.wantValid && (!got.Time.Equal(tc.wantUTC) || got.Time.Location() != time.UTC) {
				t.Fatalf("Time = %v (%v), want UTC %v", got.Time, got.Time.Location(), tc.wantUTC)
			}
		})
	}
}

func timeEq(p pgtype.Timestamptz, want time.Time) bool { return p.Valid && p.Time.Equal(want) }

// Board lane, stored opened -> observed closed: ObservedAt is taken after the fetch
// returns and before the first side effect (the rework cancel), and ProviderAt is ClosedAt.
func TestBoardLaneCloseEdgeCapturesObservationBeforeSideEffects(t *testing.T) {
	runID, repoID := uuid.New(), uuid.New()
	closedAt := time.Date(2026, 3, 2, 11, 0, 0, 0, time.UTC)
	st := &fakeStore{
		candidates: []store.ListMRWatchCandidatesRow{candidate(runID, 9, 13, mrTxt("opened"))},
		issue:      mrIssue(repoID, 9, "opened", "PRD", board.ColumnHumanReview),
		columns:    mrCols(),
	}
	mr := forgeMR(13, forge.MRStateClosed)
	mr.ClosedAt = &closedAt
	f := &fakeForge{mr: mr}
	canceller := &fakeReworkCanceller{}
	svc := newTestService(st)
	svc.SetReworkCanceller(canceller)

	if err := svc.SyncMRStates(context.Background(), repoID, testProjectID, f, f); err != nil {
		t.Fatalf("SyncMRStates: %v", err)
	}
	assertRecorded(t, st, runID, forge.MRStateClosed)
	if len(f.mrReturnedAt) != 1 || len(canceller.started) != 1 || len(f.updateStartedAt) != 1 {
		t.Fatalf("markers: fetch=%d cancel=%d move=%d", len(f.mrReturnedAt), len(canceller.started), len(f.updateStartedAt))
	}
	w := st.mrStateWrites[0]
	if !w.ObservedAt.Valid || w.ObservedAt.Time.Before(f.mrReturnedAt[0]) {
		t.Fatalf("ObservedAt %v is before the fetch returned at %v", w.ObservedAt, f.mrReturnedAt[0])
	}
	first := canceller.started[0]
	if f.updateStartedAt[0].Before(first) {
		first = f.updateStartedAt[0]
	}
	if !w.ObservedAt.Time.Before(first) {
		t.Fatalf("ObservedAt %v is not before the first side effect at %v", w.ObservedAt.Time, first)
	}
	if !timeEq(w.ProviderAt, closedAt) {
		t.Fatalf("ProviderAt = %+v, want %v", w.ProviderAt, closedAt)
	}
}

// Board-free lane, stored opened -> observed merged: the same ordering against the
// cancel, with ProviderAt equal to MergedAt.
func TestBoardFreeTerminalEdgeCapturesObservationBeforeCancel(t *testing.T) {
	runID, repoID := uuid.New(), uuid.New()
	mergedAt := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	closedAt := mergedAt.Add(time.Second)
	st := &fakeStore{boardFreeCandidates: []store.ListBoardFreeMRStateWatchCandidatesRow{
		boardFreeCand(runID, 13, strptr(forge.MRStateOpened)),
	}}
	mr := forgeMR(13, forge.MRStateMerged)
	mr.MergedAt, mr.ClosedAt = &mergedAt, &closedAt
	f := &fakeForge{mr: mr}
	canceller := &fakeReworkCanceller{}
	svc := newTestService(st)
	svc.SetReworkCanceller(canceller)

	if err := svc.SyncBoardFreeMRStates(context.Background(), repoID, testProjectID, f); err != nil {
		t.Fatalf("SyncBoardFreeMRStates: %v", err)
	}
	assertRecorded(t, st, runID, forge.MRStateMerged)
	if len(f.mrReturnedAt) != 1 || len(canceller.started) != 1 {
		t.Fatalf("markers: fetch=%d cancel=%d", len(f.mrReturnedAt), len(canceller.started))
	}
	w := st.mrStateWrites[0]
	if !w.ObservedAt.Valid || w.ObservedAt.Time.Before(f.mrReturnedAt[0]) {
		t.Fatalf("ObservedAt %v is before the fetch returned at %v", w.ObservedAt, f.mrReturnedAt[0])
	}
	if !w.ObservedAt.Time.Before(canceller.started[0]) {
		t.Fatalf("ObservedAt %v is not before the cancel start %v", w.ObservedAt.Time, canceller.started[0])
	}
	if !timeEq(w.ProviderAt, mergedAt) {
		t.Fatalf("ProviderAt = %+v, want MergedAt %v (ClosedAt must be ignored)", w.ProviderAt, mergedAt)
	}
}

// A failed write leaves mr_state as it was, so the next eligible poll observes the
// same edge again and hands over the provider time and a fresh observation time. The
// fake persists successful writes, so without the failure the second tick sees the
// state as already recorded and writes nothing (the control subtest).
func TestFailedHistoryWriteIsRetriedWithTimestamps(t *testing.T) {
	mergedAt := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	run := func(t *testing.T, failOnce error) (*fakeStore, *fakeForge) {
		t.Helper()
		runID, repoID := uuid.New(), uuid.New()
		opened := forge.MRStateOpened
		st := &fakeStore{
			boardFreeCandidates: []store.ListBoardFreeMRStateWatchCandidatesRow{boardFreeCand(runID, 13, &opened)},
			mrStateFailOnce:     failOnce,
			persistMRState:      true,
		}
		mr := forgeMR(13, forge.MRStateMerged)
		mr.MergedAt = &mergedAt
		f := &fakeForge{mr: mr}
		svc := newTestService(st)
		for i := 0; i < 2; i++ {
			if err := svc.SyncBoardFreeMRStates(context.Background(), repoID, testProjectID, f); err != nil {
				t.Fatalf("tick %d: %v", i, err)
			}
		}
		return st, f
	}

	t.Run("failed write is retried", func(t *testing.T) {
		st, f := run(t, errors.New("transient"))
		if len(st.mrStateWrites) != 2 || len(f.mrReturnedAt) != 2 {
			t.Fatalf("expected the failed write and one retry over two fetches, got writes=%d fetches=%d", len(st.mrStateWrites), len(f.mrReturnedAt))
		}
		for i, w := range st.mrStateWrites {
			if !timeEq(w.ProviderAt, mergedAt) {
				t.Fatalf("write %d ProviderAt not carried: %+v", i, w.ProviderAt)
			}
			// Each write's observation time is taken after ITS OWN fetch returned: a retry
			// that reused the first attempt's ObservedAt would be before the second marker.
			if !w.ObservedAt.Valid || w.ObservedAt.Time.Before(f.mrReturnedAt[i]) {
				t.Fatalf("write %d ObservedAt %+v is before its fetch returned at %v", i, w.ObservedAt, f.mrReturnedAt[i])
			}
		}
	})
	t.Run("control: successful write is not repeated", func(t *testing.T) {
		st, _ := run(t, nil)
		if len(st.mrStateWrites) != 1 {
			t.Fatalf("expected exactly one write without a failure, got %d", len(st.mrStateWrites))
		}
	})
}

// A failed rework cancel leaves mr_state unadvanced; the next eligible poll with a
// working cancel records the same terminal edge, with its provider time.
func TestCancelFailureThenRetryRecordsEdge(t *testing.T) {
	runID, repoID := uuid.New(), uuid.New()
	mergedAt := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	st := &fakeStore{
		boardFreeCandidates: []store.ListBoardFreeMRStateWatchCandidatesRow{boardFreeCand(runID, 13, strptr(forge.MRStateOpened))},
		persistMRState:      true,
	}
	mr := forgeMR(13, forge.MRStateMerged)
	mr.MergedAt = &mergedAt
	f := &fakeForge{mr: mr}
	canceller := &fakeReworkCanceller{err: errors.New("cancel failed")}
	svc := newTestService(st)
	svc.SetReworkCanceller(canceller)

	if err := svc.SyncBoardFreeMRStates(context.Background(), repoID, testProjectID, f); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	if len(st.mrStateWrites) != 0 {
		t.Fatalf("a failed cancel must not record, got %d writes", len(st.mrStateWrites))
	}
	canceller.err = nil
	if err := svc.SyncBoardFreeMRStates(context.Background(), repoID, testProjectID, f); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	assertRecorded(t, st, runID, forge.MRStateMerged)
	if len(f.mrReturnedAt) != 2 || len(canceller.started) != 2 {
		t.Fatalf("markers: fetch=%d cancel=%d", len(f.mrReturnedAt), len(canceller.started))
	}
	w := st.mrStateWrites[0]
	if !timeEq(w.ProviderAt, mergedAt) || w.ObservedAt.Time.Before(f.mrReturnedAt[1]) {
		t.Fatalf("retry write = %+v, want ProviderAt %v and ObservedAt after the second fetch %v", w, mergedAt, f.mrReturnedAt[1])
	}
}
