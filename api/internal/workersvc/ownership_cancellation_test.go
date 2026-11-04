package workersvc

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// ownershipQueryStore adds the optional GetRunOwnershipByWorker query to fakeStore, the way
// *store.Queries has it, so RunOwnership takes the signal-bearing read.
type ownershipQueryStore struct {
	*fakeStore
	row store.GetRunOwnershipByWorkerRow
	err error
}

func (s *ownershipQueryStore) GetRunOwnershipByWorker(context.Context, store.GetRunOwnershipByWorkerParams) (store.GetRunOwnershipByWorkerRow, error) {
	return s.row, s.err
}

// Issue #3860: a store without GetRunOwnershipByWorker (the unit fakes) falls back to
// GetRunOwnedByWorker and reports cancellationRequested=false, even for a stamped run.
func TestRunOwnershipFallbackReportsNoCancellation(t *testing.T) {
	run := store.Run{ID: uuid.New(), Kind: "issue", Status: "running", ClaimGeneration: 5,
		StopKind: pgtype.Text{String: "cancelled", Valid: true}}
	s := New(&fakeStore{runOwned: run}, nil, testParams())
	status, _, gen, _, _, _, cancelled, err := s.RunOwnership(context.Background(), store.Worker{ID: uuid.New()}, run.ID)
	if err != nil || status != "running" || gen != 5 || cancelled {
		t.Fatalf("RunOwnership = %q, %d, cancellationRequested=%v, %v; want running, 5, false, nil", status, gen, cancelled, err)
	}
}

// With the query available, the signal rides the same row and the Go-side ownership refusals
// (cross_check at generation 0, lane mismatch, no row) still apply.
func TestRunOwnershipQueryPathSignalAndRefusals(t *testing.T) {
	id := uuid.New()
	issue := store.Run{ID: id, Kind: "issue", Status: "running", ClaimGeneration: 2}
	for name, tc := range map[string]struct {
		row     store.GetRunOwnershipByWorkerRow
		err     error
		wkr     store.Worker
		want    bool
		wantErr error
	}{
		"signal true":          {row: store.GetRunOwnershipByWorkerRow{Run: issue, CancellationRequested: true}, want: true},
		"signal false":         {row: store.GetRunOwnershipByWorkerRow{Run: issue}},
		"no row":               {err: pgx.ErrNoRows, wantErr: ErrRunNotOwned},
		"cross_check gen 0":    {row: store.GetRunOwnershipByWorkerRow{Run: store.Run{ID: id, Kind: "cross_check"}, CancellationRequested: true}, wantErr: ErrRunNotOwned},
		"lane mismatch":        {row: store.GetRunOwnershipByWorkerRow{Run: issue, CancellationRequested: true}, wkr: store.Worker{IsolatedLane: true}, wantErr: ErrRunNotOwned},
		"unrelated query fail": {err: errors.New("boom"), wantErr: errors.New("boom")},
	} {
		tc.wkr.ID = uuid.New()
		s := New(&ownershipQueryStore{fakeStore: &fakeStore{}, row: tc.row, err: tc.err}, nil, testParams())
		_, _, _, _, _, _, got, err := s.RunOwnership(context.Background(), tc.wkr, id)
		switch {
		case tc.wantErr == ErrRunNotOwned && !errors.Is(err, ErrRunNotOwned):
			t.Errorf("%s: err = %v, want ErrRunNotOwned", name, err)
		case tc.wantErr != nil && tc.wantErr != ErrRunNotOwned && (err == nil || err.Error() != tc.wantErr.Error()):
			t.Errorf("%s: err = %v, want %v", name, err, tc.wantErr)
		case tc.wantErr == nil && (err != nil || got != tc.want):
			t.Errorf("%s: cancellationRequested = %v, err = %v; want %v, nil", name, got, err, tc.want)
		case tc.wantErr != nil && got:
			t.Errorf("%s: cancellationRequested = true on an error", name)
		}
	}
}
