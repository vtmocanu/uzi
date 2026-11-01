package recovery

import (
	"context"
	"encoding/json"
	"strings"
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
	worker := store.Worker{ID: uuid.New(), UserID: uuid.New(), ProtocolCapabilities: []string{capability.RecoveryBindinglessClosedV1}}
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
	worker.ProtocolCapabilities = []string{capability.RecoveryHeldPublicationV2}
	if observe(base) == nil {
		t.Fatal("V2 compatibility refused")
	}
	worker.ProtocolCapabilities = []string{capability.RecoveryBindinglessClosedV1}
	if observe(base) == nil {
		t.Fatal("valid exact open observation refused")
	}
	for name, mutate := range map[string]func(*apitypes.RecoveryExactHold){
		"wrong run":                func(v *apitypes.RecoveryExactHold) { v.RunID = uuid.NewString() },
		"wrong generation":         func(v *apitypes.RecoveryExactHold) { v.Generation = 2 },
		"wrong original custodian": func(v *apitypes.RecoveryExactHold) { v.OriginalWorkerID = uuid.NewString() },
		"blank identity":           func(v *apitypes.RecoveryExactHold) { v.OriginalWorkerIdentity = "  " },
		"regressed claim":          func(v *apitypes.RecoveryExactHold) { v.CurrentClaimGeneration = 0 },
		"invalid run worker":       func(v *apitypes.RecoveryExactHold) { v.RunWorkerID = "bad" },
		"unsupported status":       func(v *apitypes.RecoveryExactHold) { v.RunStatus = "invented" },
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
	for _, generation := range []int64{0, -1} {
		if out, err := svc.ObserveExactHold(context.Background(), worker, run, generation); out != nil || err != nil || db.calls != before {
			t.Fatal("invalid generation queried database")
		}
	}
	worker.ProtocolCapabilities = []string{capability.RecoveryHeldPublicationV1}
	if out, err := svc.ObserveExactHold(context.Background(), worker, run, 1); out != nil || err != nil || db.calls != before {
		t.Fatal("V1 reached widened query")
	}
}

func TestObserveExactHoldClosedProofs(t *testing.T) {
	w := store.Worker{ID: uuid.New(), UserID: uuid.New(), ProtocolCapabilities: []string{capability.RecoveryBindinglessClosedV1}}
	run := uuid.New()
	db := &exactObservationDB{}
	svc := New(nil, db, nil, Limits{}, nil)
	base := apitypes.RecoveryExactHold{RunID: run.String(), HoldID: uuid.NewString(), RepoID: uuid.NewString(),
		Generation: 1, OriginalWorkerID: w.ID.String(), OriginalWorkerIdentity: "saved label",
		InventoryGuarded: true, State: "released", RunStatus: "completed", CurrentClaimGeneration: 9, RunWorkerID: uuid.NewString()}
	iid := int64(7)
	receipt := apitypes.CompletedPublicationReceipt{CompletionIdentity: apitypes.CompletionIdentity{
		HoldID: base.HoldID, RunID: base.RunID, OwnerID: w.UserID.String(), WorkerID: w.ID.String(),
		Generation: 1, RepoID: base.RepoID, ConnectionID: uuid.NewString(), ProjectID: 1,
		ForgeType: "github", BaseURL: "https://github.com", Branch: "work", MRIID: &iid, FinalHead: strings.Repeat("a", 40)},
		ObservedBranchHead: strings.Repeat("b", 40)}
	observe := func(v apitypes.RecoveryExactHold, want bool) {
		t.Helper()
		var err error
		db.raw, err = json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		out, err := svc.ObserveExactHold(context.Background(), w, run, 1)
		if err != nil || (out != nil) != want {
			t.Fatalf("observation=%+v err=%v want known=%t", out, err, want)
		}
	}
	cases := []struct {
		kind, evidence string
		final          apitypes.RecoveryFinalDisposition
	}{
		{"held_publication", "held_publication", apitypes.RecoveryFinalDisposition{Kind: "held_publication", PublicationID: uuid.NewString(), SourceSha: strings.Repeat("a", 40), CoverageDigest: strings.Repeat("b", 64)}},
		{"archive", "archive", apitypes.RecoveryFinalDisposition{Kind: "archive", CaptureID: uuid.NewString(), SourceSha: "aaaa1111", CoverageDigest: strings.Repeat("b", 64)}},
		{"closure", "prerequisite_closure", apitypes.RecoveryFinalDisposition{Kind: "archive", CaptureID: uuid.NewString(), SourceSha: "aaaa1111", CoverageDigest: strings.Repeat("b", 64)}},
		{"settled", "publication", apitypes.RecoveryFinalDisposition{Kind: "settled", CoverageDigest: emptyInventoryDigest}},
		{"settled no output", "forge_no_output", apitypes.RecoveryFinalDisposition{Kind: "settled", CoverageDigest: emptyInventoryDigest}},
		{"no source", "no_adopted_source", apitypes.RecoveryFinalDisposition{Kind: "no_adopted_source"}},
		{"completed", "completed_publication", apitypes.RecoveryFinalDisposition{Kind: "completed_publication"}},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			v := base
			f := tc.final
			v.FinalReceipt = &f
			v.ReleaseEvidence = tc.evidence
			if tc.kind == "completed" {
				r := receipt
				v.CompletedPublicationReceipt = &r
			}
			observe(v, true)
			if tc.kind == "completed" {
				bad := v
				bad.CompletedPublicationReceipt = nil
				observe(bad, false)
			}
			bad := v
			bad.ReleaseEvidence = "unsupported"
			observe(bad, false)
			bad = v
			bf := f
			bf.Kind = "future"
			bad.FinalReceipt = &bf
			observe(bad, false)
			bad = v
			bf = f
			bf.CoverageDigest = "malformed"
			bad.FinalReceipt = &bf
			observe(bad, false)
			bad = v
			bf = f
			bf.SourceSha = "forged"
			bad.FinalReceipt = &bf
			observe(bad, false)
			bad = v
			bf = f
			bf.CaptureID = "bad"
			bad.FinalReceipt = &bf
			observe(bad, false)
			bad = v
			bf = f
			bf.PublicationID = "bad"
			bad.FinalReceipt = &bf
			observe(bad, false)
			bad = v
			bad.LiveWorkerID = w.ID.String()
			observe(bad, false)
			bad = v
			bad.CompletedPublicationReceipt = &receipt
			if tc.kind != "completed" {
				observe(bad, false)
			}
		})
	}
	for name, change := range map[string]func(*apitypes.CompletedPublicationReceipt){
		"hold":          func(r *apitypes.CompletedPublicationReceipt) { r.HoldID = uuid.NewString() },
		"run":           func(r *apitypes.CompletedPublicationReceipt) { r.RunID = uuid.NewString() },
		"owner":         func(r *apitypes.CompletedPublicationReceipt) { r.OwnerID = uuid.NewString() },
		"worker":        func(r *apitypes.CompletedPublicationReceipt) { r.WorkerID = uuid.NewString() },
		"repo":          func(r *apitypes.CompletedPublicationReceipt) { r.RepoID = uuid.NewString() },
		"generation":    func(r *apitypes.CompletedPublicationReceipt) { r.Generation++ },
		"connection":    func(r *apitypes.CompletedPublicationReceipt) { r.ConnectionID = "bad" },
		"project":       func(r *apitypes.CompletedPublicationReceipt) { r.ProjectID = 0 },
		"mr":            func(r *apitypes.CompletedPublicationReceipt) { r.MRIID = nil },
		"branch":        func(r *apitypes.CompletedPublicationReceipt) { r.Branch = "" },
		"forge":         func(r *apitypes.CompletedPublicationReceipt) { r.ForgeType = "unknown" },
		"head":          func(r *apitypes.CompletedPublicationReceipt) { r.FinalHead = "bad" },
		"observed head": func(r *apitypes.CompletedPublicationReceipt) { r.ObservedBranchHead = "bad" },
		"http":          func(r *apitypes.CompletedPublicationReceipt) { r.BaseURL = "http://github.com" },
		"userinfo":      func(r *apitypes.CompletedPublicationReceipt) { r.BaseURL = "https://user@github.com" },
		"query":         func(r *apitypes.CompletedPublicationReceipt) { r.BaseURL = "https://github.com?x=1" },
		"fragment":      func(r *apitypes.CompletedPublicationReceipt) { r.BaseURL = "https://github.com/#x" },
	} {
		t.Run("completed "+name, func(t *testing.T) {
			v := base
			r := receipt
			change(&r)
			v.CompletedPublicationReceipt = &r
			v.ReleaseEvidence = "completed_publication"
			v.FinalReceipt = &apitypes.RecoveryFinalDisposition{Kind: "completed_publication"}
			observe(v, false)
		})
	}
	for _, evidence := range []string{"owner_discard", "merged_predecessor_discard"} {
		v := base
		v.State = "discarded"
		v.ReleaseEvidence = evidence
		observe(v, true)
	}
	db.raw = []byte("{")
	if out, err := svc.ObserveExactHold(context.Background(), w, run, 1); out != nil || err != nil {
		t.Fatalf("malformed JSON: %v %v", out, err)
	}
}
