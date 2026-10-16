package recovery

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type exactObservationDB struct {
	DB
	raw   []byte
	calls int
}

func (d *exactObservationDB) QueryRow(context.Context, string, ...any) pgx.Row {
	d.calls++
	return exactObservationRow{raw: d.raw}
}

type exactObservationRow struct{ raw []byte }

func (r exactObservationRow) Scan(dest ...any) error { *(dest[0].(*[]byte)) = r.raw; return nil }

// Parser tests use the public observation seam. SQL scope is exercised by the handler LiveDB test.
func TestObserveExactHoldRejectsMalformedEvidence(t *testing.T) {
	worker := store.Worker{ID: uuid.New(), UserID: uuid.New(), ProtocolCapabilities: []string{capability.RecoveryHeldPublicationV2}}
	run, hold, repo := uuid.New(), uuid.New(), uuid.New()
	db := &exactObservationDB{}
	svc := New(nil, db, nil, Limits{}, nil)
	base := apitypes.RecoveryExactHold{RunID: run.String(), HoldID: hold.String(), RepoID: repo.String(), Generation: 1,
		OriginalWorkerID: worker.ID.String(), OriginalWorkerIdentity: "original", InventoryGuarded: true,
		State: "open", LiveWorkerID: worker.ID.String(), LiveRunID: run.String(), RunStatus: "failed", CurrentClaimGeneration: 1}
	observe := func(value apitypes.RecoveryExactHold) *apitypes.RecoveryExactHold {
		t.Helper()
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		db.raw = raw
		out, err := svc.ObserveExactHold(context.Background(), worker, run, 1)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if observe(base) == nil {
		t.Fatal("valid exact open observation refused")
	}
	for name, mutate := range map[string]func(*apitypes.RecoveryExactHold){
		"wrong run":                func(v *apitypes.RecoveryExactHold) { v.RunID = uuid.NewString() },
		"wrong generation":         func(v *apitypes.RecoveryExactHold) { v.Generation = 2 },
		"wrong original custodian": func(v *apitypes.RecoveryExactHold) { v.OriginalWorkerID = uuid.NewString() },
		"unguarded":                func(v *apitypes.RecoveryExactHold) { v.InventoryGuarded = false },
		"missing repository":       func(v *apitypes.RecoveryExactHold) { v.RepoID = "" },
		"foreign live custodian":   func(v *apitypes.RecoveryExactHold) { v.LiveWorkerID = uuid.NewString() },
		"closed without evidence":  func(v *apitypes.RecoveryExactHold) { v.State = "released"; v.LiveWorkerID = ""; v.LiveRunID = "" },
	} {
		t.Run(name, func(t *testing.T) {
			v := base
			mutate(&v)
			if observe(v) != nil {
				t.Fatal("malformed evidence accepted")
			}
		})
	}
	closed := base
	closed.State = "released"
	closed.LiveWorkerID = ""
	closed.LiveRunID = ""
	closed.ReleaseEvidence = "held_publication"
	closed.FinalReceipt = &apitypes.RecoveryFinalDisposition{Kind: "held_publication", PublicationID: uuid.NewString(), SourceSha: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", CoverageDigest: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}
	if out := observe(closed); out == nil || out.FinalReceipt.PublicationID != closed.FinalReceipt.PublicationID {
		t.Fatal("saved held FINAL identity lost")
	}
	closed.FinalReceipt.PublicationID = "bad"
	if observe(closed) != nil {
		t.Fatal("malformed saved FINAL accepted")
	}
	before := db.calls
	worker.ProtocolCapabilities = []string{capability.RecoveryHeldPublicationV1}
	if out, err := svc.ObserveExactHold(context.Background(), worker, run, 1); out != nil || err != nil || db.calls != before {
		t.Fatal("V1 reached widened query")
	}
}
