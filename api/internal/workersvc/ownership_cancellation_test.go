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

// Issue #3937: GetRunOwnershipByWorker is a required Store query, so RunOwnership reports exactly
// the signal the query returns. A run stamped stop_kind='cancelled' whose query signal is false
// (no deliverable cancel input) reports false: the Go side never infers it from stop_kind.
func TestRunOwnershipExplicitFalseSignal(t *testing.T) {
	run := store.Run{ID: uuid.New(), Kind: "issue", Status: "running", ClaimGeneration: 5,
		StopKind: pgtype.Text{String: "cancelled", Valid: true}}
	s := New(&fakeStore{runOwned: run}, nil, testParams())
	status, _, gen, _, _, _, cancelled, err := s.RunOwnership(context.Background(), store.Worker{ID: uuid.New()}, run.ID)
	if err != nil || status != "running" || gen != 5 || cancelled {
		t.Fatalf("RunOwnership = %q, %d, cancellationRequested=%v, %v; want running, 5, false, nil", status, gen, cancelled, err)
	}
}

// The signal rides the query's row, and the Go-side ownership refusals (cross_check at
// generation 0, lane mismatch, no row) and query errors still apply.
func TestRunOwnershipQuerySignalAndRefusals(t *testing.T) {
	id := uuid.New()
	issue := store.Run{ID: id, Kind: "issue", Status: "running", ClaimGeneration: 2}
	stamped := issue
	stamped.StopKind = pgtype.Text{String: "cancelled", Valid: true}
	for name, tc := range map[string]struct {
		run       store.Run
		requested bool
		err       error
		wkr       store.Worker
		want      bool
		wantErr   error
	}{
		"signal true":                 {run: stamped, requested: true, want: true},
		"signal false":                {run: stamped},
		"signal true, NULL stop_kind": {run: issue, requested: true, want: true},
		"no row":                      {err: pgx.ErrNoRows, requested: true, wantErr: ErrRunNotOwned},
		"cross_check gen 0":           {run: store.Run{ID: id, Kind: "cross_check"}, requested: true, wantErr: ErrRunNotOwned},
		"lane mismatch":               {run: stamped, requested: true, wkr: store.Worker{IsolatedLane: true}, wantErr: ErrRunNotOwned},
		"unrelated query fail":        {err: errors.New("boom"), requested: true, wantErr: errors.New("boom")},
	} {
		tc.wkr.ID = uuid.New()
		fs := &fakeStore{runOwned: tc.run, runOwnedErr: tc.err, cancellationRequested: tc.requested}
		s := New(fs, nil, testParams())
		_, _, _, _, _, _, got, err := s.RunOwnership(context.Background(), tc.wkr, id)
		switch {
		case fs.ownershipParams == nil:
			t.Errorf("%s: GetRunOwnershipByWorker was not called", name)
		case fs.ownershipParams.ID != id || fs.ownershipParams.WorkerID.Bytes != tc.wkr.ID:
			t.Errorf("%s: query params = %+v, want run %s worker %s", name, *fs.ownershipParams, id, tc.wkr.ID)
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
