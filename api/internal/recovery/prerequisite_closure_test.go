package recovery

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type closureStatusFake struct {
	Store
	capture store.RecoveryCapture
	row     store.RecoveryPrerequisiteClosure
	err     error
	reads   int
}

func (f *closureStatusFake) GetCaptureForOwner(context.Context, store.GetCaptureForOwnerParams) (store.RecoveryCapture, error) {
	return f.capture, nil
}
func (f *closureStatusFake) GetPrerequisiteClosure(context.Context, store.GetPrerequisiteClosureParams) (store.RecoveryPrerequisiteClosure, error) {
	f.reads++
	return f.row, f.err
}

func TestRecoveryPrerequisiteClosureStatus(t *testing.T) {
	for _, tc := range []struct {
		name, rowState, rowReason, wantState, wantReason string
		bound, empty, noRow, absent, omit                bool
	}{
		{name: "unbound", noRow: true, wantState: "unsupported", wantReason: "closure_unsupported"},
		{name: "unbound ignores unavailable manifest", empty: true, noRow: true, wantState: "unsupported", wantReason: "closure_unsupported"},
		{name: "self contained", bound: true, empty: true, noRow: true, omit: true},
		{name: "bound no row", bound: true, noRow: true, wantState: "unsupported", wantReason: "closure_unsupported"},
		{name: "pending", bound: true, rowState: "pending", wantState: "pending"},
		{name: "ancestry retry", bound: true, rowState: "pending", rowReason: "prerequisite_ancestry_unknown", wantState: "pending", wantReason: "prerequisite_ancestry_unknown"},
		{name: "pending hides terminal budget", bound: true, rowState: "pending", rowReason: "prerequisite_prepare_budget_exhausted", wantState: "pending"},
		{name: "invoked", bound: true, rowState: "invoked", wantState: "pending", wantReason: "pin_unknown"},
		{name: "unknown", bound: true, rowState: "create_unknown", wantState: "pending", wantReason: "pin_unknown"},
		{name: "pinned", bound: true, rowState: "pinned", wantState: "pinned"},
		{name: "pinned cleanup exhausted stays owner only", bound: true, rowState: "pinned", rowReason: "pin_cleanup_exhausted", wantState: "pinned"},
		{name: "absence", bound: true, rowState: "pinned", absent: true, wantState: "pending", wantReason: "pin_absence_unconfirmed"},
		{name: "refused", bound: true, rowState: "refused", rowReason: "pin_refused", wantState: "refused", wantReason: "pin_refused"},
		{name: "foreign", bound: true, rowState: "foreign", wantState: "refused", wantReason: "pin_foreign"},
		{name: "lost", bound: true, rowState: "lost", wantState: "lost", wantReason: "pin_lost"},
		{name: "deleted", bound: true, rowState: "deleted", wantState: "unsupported", wantReason: "archive_unavailable"},
		{name: "cleanup hidden", bound: true, rowState: "refused", rowReason: "pin_cleanup_exhausted", wantState: "refused", wantReason: "closure_unsupported"},
		{name: "raw reason hidden", bound: true, rowState: "refused", rowReason: "raw provider secret", wantState: "refused", wantReason: "closure_unsupported"},
		{name: "not on default", bound: true, rowState: "refused", rowReason: "prerequisite_not_on_default", wantState: "refused", wantReason: "prerequisite_not_on_default"},
		{name: "budget exhausted", bound: true, rowState: "refused", rowReason: "prerequisite_prepare_budget_exhausted", wantState: "refused", wantReason: "prerequisite_prepare_budget_exhausted"},
		{name: "archive unavailable", bound: true, rowState: "refused", rowReason: "archive_unavailable", wantState: "refused", wantReason: "archive_unavailable"},
		{name: "unsupported", bound: true, rowState: "refused", rowReason: "closure_unsupported", wantState: "refused", wantReason: "closure_unsupported"},
		{name: "refused ancestry", bound: true, rowState: "refused", rowReason: "prerequisite_ancestry_unknown", wantState: "refused", wantReason: "prerequisite_ancestry_unknown"},
		{name: "refused absence", bound: true, rowState: "refused", rowReason: "pin_absence_unconfirmed", wantState: "refused", wantReason: "pin_absence_unconfirmed"},
		{name: "refused unknown", bound: true, rowState: "refused", rowReason: "pin_unknown", wantState: "refused", wantReason: "pin_unknown"},
		{name: "refused foreign", bound: true, rowState: "refused", rowReason: "pin_foreign", wantState: "refused", wantReason: "pin_foreign"},
		{name: "refused lost", bound: true, rowState: "refused", rowReason: "pin_lost", wantState: "refused", wantReason: "pin_lost"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := store.Worker{ID: uuid.New(), UserID: uuid.New()}
			f := &closureStatusFake{capture: store.RecoveryCapture{ID: uuid.New(), RunID: uuid.New(), UserID: w.UserID, OriginalWorkerID: pgtype.UUID{Bytes: w.ID, Valid: true}, ManifestBound: tc.bound, PrerequisiteShas: []string{"capture-sha"}, Checksum: pgtype.Text{String: "capture-checksum", Valid: true}},
				row: store.RecoveryPrerequisiteClosure{State: tc.rowState, Reason: tc.rowReason, LastError: "secret provider detail", Checksum: "frozen-checksum", PrerequisiteShas: []string{"frozen-sha"}, AbsentSince: pgtype.Timestamptz{Valid: tc.absent}}}
			if tc.empty {
				f.capture.PrerequisiteShas = nil
			}
			if tc.noRow {
				f.err = pgx.ErrNoRows
			}
			s := New(f, nil, nil, Limits{}, nil)
			got, err := s.Status(context.Background(), w, f.capture.RunID, f.capture.ID)
			if err != nil {
				t.Fatal(err)
			}
			c := got.PrerequisiteClosure
			if tc.omit {
				if c != nil || f.reads != 0 {
					t.Fatalf("self contained: %+v reads %d", c, f.reads)
				}
				return
			}
			if c == nil || c.State != tc.wantState || c.Reason != tc.wantReason {
				t.Fatalf("status: %+v", c)
			}
			if !tc.noRow {
				if c.Checksum != "frozen-checksum" || !reflect.DeepEqual(c.PrerequisiteShas, []string{"frozen-sha"}) {
					t.Fatalf("row facts: %+v", c)
				}
				c.PrerequisiteShas[0] = "changed"
				if f.row.PrerequisiteShas[0] != "frozen-sha" {
					t.Fatal("response aliases frozen row")
				}
			} else if tc.bound {
				if c.Checksum != "capture-checksum" || !reflect.DeepEqual(c.PrerequisiteShas, []string{"capture-sha"}) {
					t.Fatalf("manifest facts: %+v", c)
				}
			} else if c.Checksum != "" || len(c.PrerequisiteShas) != 0 {
				t.Fatalf("unbound facts: %+v", c)
			}
		})
	}
}

func TestRecoveryPrerequisiteClosureStatusReadFailure(t *testing.T) {
	w := store.Worker{ID: uuid.New()}
	failure := errors.New("database unavailable")
	f := &closureStatusFake{capture: store.RecoveryCapture{OriginalWorkerID: pgtype.UUID{Bytes: w.ID, Valid: true}}, err: failure}
	if _, err := New(f, nil, nil, Limits{}, nil).Status(context.Background(), w, uuid.New(), uuid.New()); !errors.Is(err, failure) {
		t.Fatal(err)
	}
}
