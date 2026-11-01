package recovery

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type ownerSummaryStore struct {
	Store
	rows            []store.ListCapturesForRunOwnerRow
	sumErr, listErr error
	calls           []string
}

func (f *ownerSummaryStore) GetRecoverySummaryForRun(context.Context, store.GetRecoverySummaryForRunParams) (store.GetRecoverySummaryForRunRow, error) {
	f.calls = append(f.calls, "aggregate")
	return store.GetRecoverySummaryForRunRow{Supported: true}, f.sumErr
}
func (f *ownerSummaryStore) ListCapturesForRunOwner(context.Context, store.ListCapturesForRunOwnerParams) ([]store.ListCapturesForRunOwnerRow, error) {
	f.calls = append(f.calls, "listing")
	return f.rows, f.listErr
}

// TestOwnerSummarySharedClosureCases exercises the public Summary seam. The semantic
// expectations are checked against projected JSON independently of the row inputs.
func TestOwnerSummarySharedClosureCases(t *testing.T) {
	raw, err := os.ReadFile("../../../fixtures/recovery-prerequisite-closure/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var table struct {
		PinRef    string    `json:"pin_ref"`
		PinTip    string    `json:"pin_tip"`
		UpdatedAt time.Time `json:"updated_at"`
		Cases     []struct {
			Name  string `json:"name"`
			Input struct {
				Row               bool `json:"row"`
				State, Reason     string
				Thin              bool
				CaptureState      string `json:"capture_state"`
				PinTip            bool   `json:"pin_tip"`
				Absent, Exhausted bool
			}
			Expected struct {
				Closure             *apitypes.RecoveryPrerequisiteClosureDTO
				Protection, Cleanup string
				PermittedActions    []string `json:"permitted_actions"`
			}
		}
	}
	if err := json.Unmarshal(raw, &table); err != nil {
		t.Fatal(err)
	}
	wantNames := []string{"pending", "invoked", "create_unknown", "pinned", "refused", "foreign", "lost", "deleted",
		"absence-unconfirmed", "cleanup-exhausted", "both-flags", "deleted-historical-exhaustion",
		"thin-no-row", "self-contained", "old-server", "unknown-reason", "non-pinned-absence", "nullable-id-missing", "unknown-state"}
	var names []string
	for _, tc := range table.Cases {
		names = append(names, tc.Name)
	}
	if !reflect.DeepEqual(names, wantNames) {
		t.Fatalf("shared semantic case names=%v want %v", names, wantNames)
	}
	for _, tc := range table.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			row := store.ListCapturesForRunOwnerRow{
				RecoveryCapture:    store.RecoveryCapture{ID: uuid.New(), State: tc.Input.CaptureState, ManifestBound: true},
				ClosureID:          pgtype.UUID{Bytes: uuid.New(), Valid: tc.Input.Row},
				ClosureState:       pgtype.Text{String: tc.Input.State, Valid: true},
				ClosureReason:      pgtype.Text{String: tc.Input.Reason, Valid: true},
				PinRef:             pgtype.Text{String: table.PinRef, Valid: true},
				PinTip:             pgtype.Text{String: table.PinTip, Valid: tc.Input.PinTip},
				ClosureUpdatedAt:   pgtype.Timestamptz{Time: table.UpdatedAt, Valid: true},
				AbsentSince:        pgtype.Timestamptz{Time: table.UpdatedAt, Valid: tc.Input.Absent},
				CleanupExhaustedAt: pgtype.Timestamptz{Time: table.UpdatedAt, Valid: tc.Input.Exhausted},
			}
			if tc.Input.Thin {
				row.RecoveryCapture.PrerequisiteShas = []string{"prerequisite"}
			}
			f := &ownerSummaryStore{rows: []store.ListCapturesForRunOwnerRow{row}}
			got, err := New(f, nil, nil, Limits{}, nil).Summary(context.Background(), uuid.New(), uuid.New())
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(f.calls, []string{"aggregate", "listing"}) {
				t.Fatalf("queries=%v", f.calls)
			}
			if len(got.Archives) != 1 {
				t.Fatalf("archives=%d", len(got.Archives))
			}
			archive := got.Archives[0]
			if !reflect.DeepEqual(archive.PrerequisiteClosure, tc.Expected.Closure) {
				t.Fatalf("closure=%+v want %+v", archive.PrerequisiteClosure, tc.Expected.Closure)
			}
			b, err := json.Marshal(archive)
			if err != nil {
				t.Fatal(err)
			}
			var wire map[string]json.RawMessage
			if err := json.Unmarshal(b, &wire); err != nil {
				t.Fatal(err)
			}
			_, present := wire["prerequisite_closure"]
			if present != (tc.Expected.Closure != nil) {
				t.Fatalf("optional closure presence=%v", present)
			}
			// Interpret only the output, independently of the store fixture's booleans.
			protection := "uncertain"
			cleanup := "none"
			p := archive.PrerequisiteClosure
			if len(archive.PrerequisiteShas) == 0 {
				protection = "self_contained"
			}
			if p != nil {
				switch p.State {
				case "pinned":
					if !p.AbsenceUnconfirmed {
						protection = "protected"
					}
				case "refused", "foreign", "lost", "deleted":
					protection = "unprotected"
				}
				if p.State == "deleted" {
					cleanup = "complete"
				}
				if p.State == "pinned" && (archive.State == "expired" || archive.State == "discarded") {
					cleanup = "pending"
				}
				if p.CleanupExhausted {
					cleanup = "exhausted"
				}
			}
			actions := []string{}
			if archive.State == "available" {
				actions = []string{"download", "discard"}
			}
			if protection != tc.Expected.Protection || cleanup != tc.Expected.Cleanup || !reflect.DeepEqual(actions, tc.Expected.PermittedActions) {
				t.Fatalf("semantics: protection=%s cleanup=%s actions=%v; want %s %s %v", protection, cleanup, actions, tc.Expected.Protection, tc.Expected.Cleanup, tc.Expected.PermittedActions)
			}
		})
	}
}

func TestOwnerSummaryPreservesCaptureSourcesAndOrder(t *testing.T) {
	owner, run, hold := uuid.New(), uuid.New(), uuid.New()
	first := uuid.MustParse("00000000-0000-0000-0000-000000000002")
	second := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	f := &ownerSummaryStore{rows: []store.ListCapturesForRunOwnerRow{
		{RecoveryCapture: store.RecoveryCapture{ID: first, RunID: run, HoldID: hold, State: "expired",
			SourceSha: "original-first", AttemptedHeadSha: pgtype.Text{String: "attempted-first", Valid: true},
			PrerequisiteShas: []string{"prerequisite-z", "prerequisite-a"}}},
		{RecoveryCapture: store.RecoveryCapture{ID: second, RunID: run, HoldID: hold, State: "available",
			SourceSha: "original-second", PrerequisiteShas: []string{"prerequisite-b"}}},
	}}
	got, err := New(f, nil, nil, Limits{}, nil).Summary(context.Background(), owner, run)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Archives) != 2 {
		t.Fatalf("archives=%d want 2", len(got.Archives))
	}
	a, b := got.Archives[0], got.Archives[1]
	if a.ID != first.String() || b.ID != second.String() {
		t.Fatalf("archive order=%s,%s want %s,%s", a.ID, b.ID, first, second)
	}
	if a.RunID != run.String() || a.HoldID != hold.String() || a.State != "expired" ||
		a.SourceSha != "original-first" || a.AttemptedHeadSha == nil || *a.AttemptedHeadSha != "attempted-first" {
		t.Fatalf("first capture source metadata=%+v", a)
	}
	if b.SourceSha != "original-second" || b.AttemptedHeadSha != nil {
		t.Fatalf("second capture source metadata=%+v", b)
	}
	if !reflect.DeepEqual(a.PrerequisiteShas, []string{"prerequisite-z", "prerequisite-a"}) ||
		!reflect.DeepEqual(b.PrerequisiteShas, []string{"prerequisite-b"}) {
		t.Fatalf("prerequisite arrays=%v,%v", a.PrerequisiteShas, b.PrerequisiteShas)
	}
}

func TestOwnerSummaryDatabaseFailuresStayErrors(t *testing.T) {
	failure := errors.New("database unavailable")
	for _, listing := range []bool{false, true} {
		t.Run(map[bool]string{false: "aggregate", true: "listing"}[listing], func(t *testing.T) {
			f := &ownerSummaryStore{}
			if listing {
				f.listErr = failure
			} else {
				f.sumErr = failure
			}
			_, err := New(f, nil, nil, Limits{}, nil).Summary(context.Background(), uuid.New(), uuid.New())
			if !errors.Is(err, failure) {
				t.Fatalf("error=%v want database failure", err)
			}
			want := []string{"aggregate"}
			if listing {
				want = append(want, "listing")
			}
			if !reflect.DeepEqual(f.calls, want) {
				t.Fatalf("calls=%v want %v", f.calls, want)
			}
		})
	}
}
