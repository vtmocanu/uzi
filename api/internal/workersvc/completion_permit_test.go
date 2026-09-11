package workersvc

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// --- fakeStore methods for the M2 completion queries (fields live in service_test.go) ---

func (f *fakeStore) RecordCompletionAttempt(_ context.Context, arg store.RecordCompletionAttemptParams) (int32, error) {
	f.recordedAttempts = append(f.recordedAttempts, arg)
	return f.recordAttemptCount, f.recordAttemptErr
}

func (f *fakeStore) UpsertCompletionPermit(_ context.Context, arg store.UpsertCompletionPermitParams) (store.RunCompletionPermit, error) {
	f.upsertPermitParams = &arg
	if f.upsertPermitErr != nil {
		return store.RunCompletionPermit{}, f.upsertPermitErr
	}
	p := f.upsertedPermit
	if p.ID == (uuid.UUID{}) {
		p.ID = uuid.New()
	}
	// Reflect the request into the returned row so a caller/test can assert the binding.
	p.RunID = arg.RunID
	p.ContractRevision = arg.ContractRevision
	p.Branch = arg.Branch
	p.Head = arg.Head
	p.IssuedByWorkerID = arg.IssuedByWorkerID
	return p, nil
}

// contractJSON marshals a structural contract from milestone ids for a fixture.
func contractJSON(t *testing.T, ids ...string) []byte {
	t.Helper()
	frozen := make([]Milestone, 0, len(ids))
	for _, id := range ids {
		frozen = append(frozen, Milestone{ID: id, Title: id})
	}
	fj, err := encodeJSONArray(frozen)
	if err != nil {
		t.Fatalf("encode frozen: %v", err)
	}
	c, err := buildCompletionContract(fj)
	if err != nil {
		t.Fatalf("buildCompletionContract: %v", err)
	}
	return c
}

func idsJSON(t *testing.T, ids ...string) []byte {
	t.Helper()
	out, err := encodeJSONArray(ids)
	if err != nil {
		t.Fatalf("encode ids: %v", err)
	}
	return out
}

// frozenJSON builds the runs.milestones_frozen shape ([{id,title}] objects, as DecodeMilestones
// reads), distinct from idsJSON (the milestones_completed shape, a bare id array).
func frozenJSON(t *testing.T, ids ...string) []byte {
	t.Helper()
	ms := make([]Milestone, 0, len(ids))
	for _, id := range ids {
		ms = append(ms, Milestone{ID: id, Title: id})
	}
	out, err := encodeJSONArray(ms)
	if err != nil {
		t.Fatalf("encode frozen: %v", err)
	}
	return out
}

// TestComputeUnmetCriteria pins the server-authoritative recompute, including the fail-closed
// split-state hazard.
func TestComputeUnmetCriteria(t *testing.T) {
	rev := pgtype.Int4{Int32: 1, Valid: true}
	frozen := frozenJSON(t, "m1", "m2", "m3") // used only for the fail-closed frozen-id fallback

	t.Run("some unmet", func(t *testing.T) {
		run := store.Run{
			CompletionContractVersion: rev,
			CompletionContract:        contractJSON(t, "m1", "m2", "m3"),
			MilestonesCompleted:       idsJSON(t, "m1"),
		}
		unmet, verifiable := computeUnmetCriteria(run)
		if !verifiable {
			t.Fatal("a non-null contract is verifiable")
		}
		if len(unmet) != 2 || unmet[0] != "m2" || unmet[1] != "m3" {
			t.Fatalf("unmet = %v, want [m2 m3]", unmet)
		}
	})

	t.Run("all met", func(t *testing.T) {
		run := store.Run{
			CompletionContractVersion: rev,
			CompletionContract:        contractJSON(t, "m1", "m2"),
			MilestonesCompleted:       idsJSON(t, "m1", "m2"),
		}
		unmet, verifiable := computeUnmetCriteria(run)
		if !verifiable || len(unmet) != 0 {
			t.Fatalf("unmet = %v verifiable = %v, want [] true", unmet, verifiable)
		}
	})

	t.Run("vacuously complete (empty criteria)", func(t *testing.T) {
		run := store.Run{
			CompletionContractVersion: rev,
			CompletionContract:        contractJSON(t), // criteria:[]
		}
		unmet, verifiable := computeUnmetCriteria(run)
		if !verifiable || len(unmet) != 0 {
			t.Fatalf("a milestone-less non-null contract is vacuously complete; got unmet=%v verifiable=%v", unmet, verifiable)
		}
	})

	t.Run("split-state: contract NULL but frozen -> not verifiable, all unmet", func(t *testing.T) {
		run := store.Run{
			CompletionContractVersion: rev,
			ContractRevision:          rev,
			CompletionContract:        nil, // the split-state hazard
			MilestonesFrozen:          frozen,
			MilestonesCompleted:       idsJSON(t, "m1"),
		}
		unmet, verifiable := computeUnmetCriteria(run)
		if verifiable {
			t.Fatal("a NULL contract with frozen milestones must be NOT verifiable (fail-closed)")
		}
		if len(unmet) != 3 {
			t.Fatalf("fail-closed unmet must be the full frozen set; got %v", unmet)
		}
	})

	t.Run("corrupt contract -> not verifiable", func(t *testing.T) {
		run := store.Run{
			CompletionContractVersion: rev,
			CompletionContract:        []byte("{not json"),
			MilestonesFrozen:          frozen,
		}
		_, verifiable := computeUnmetCriteria(run)
		if verifiable {
			t.Fatal("a corrupt contract must be NOT verifiable (fail-closed)")
		}
	})
}

// interlockedRun builds a running interlocked run owned by wkr with the given revision/contract.
func interlockedRun(wkr store.Worker) store.Run {
	return store.Run{
		ID:                        uuid.New(),
		WorkerID:                  pgconv.UUID(wkr.ID),
		Status:                    "running",
		CompletionContractVersion: pgtype.Int4{Int32: 1, Valid: true},
		ContractRevision:          pgtype.Int4{Int32: 1, Valid: true},
	}
}

// TestRequestCompletionPermitDenials covers every denial branch and the grant, on fakes.
func TestRequestCompletionPermitDenials(t *testing.T) {
	w := worker()
	base := CompletionPermitRequest{ContractRevision: 1, Branch: "agent/issue-1", Head: "abc123"}

	t.Run("run not owned", func(t *testing.T) {
		fs := &fakeStore{runOwnedErr: pgx.ErrNoRows}
		svc := New(fs, newBox(t), testParams())
		if _, err := svc.RequestCompletionPermit(context.Background(), w, uuid.New(), base); !errors.Is(err, ErrRunNotOwned) {
			t.Fatalf("want ErrRunNotOwned, got %v", err)
		}
	})

	t.Run("stale claim (not live)", func(t *testing.T) {
		run := interlockedRun(w)
		run.Status = "queued"
		fs := &fakeStore{runOwned: run}
		svc := New(fs, newBox(t), testParams())
		res, err := svc.RequestCompletionPermit(context.Background(), w, run.ID, base)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if res.Granted || res.DenyReason != CompletionDenyStaleClaim {
			t.Fatalf("want stale_claim denial, got %+v", res)
		}
	})

	t.Run("not interlocked (legacy)", func(t *testing.T) {
		run := interlockedRun(w)
		run.CompletionContractVersion = pgtype.Int4{} // legacy
		fs := &fakeStore{runOwned: run}
		svc := New(fs, newBox(t), testParams())
		res, _ := svc.RequestCompletionPermit(context.Background(), w, run.ID, base)
		if res.Granted || res.DenyReason != CompletionDenyNotInterlocked {
			t.Fatalf("want not_interlocked denial, got %+v", res)
		}
	})

	t.Run("revision drift", func(t *testing.T) {
		run := interlockedRun(w)
		run.CompletionContract = contractJSON(t, "m1")
		fs := &fakeStore{runOwned: run}
		svc := New(fs, newBox(t), testParams())
		req := base
		req.ContractRevision = 2 // run is at revision 1
		res, _ := svc.RequestCompletionPermit(context.Background(), w, run.ID, req)
		if res.Granted || res.DenyReason != CompletionDenyRevisionDrift {
			t.Fatalf("want revision_drift denial, got %+v", res)
		}
	})

	t.Run("contract not frozen (split state)", func(t *testing.T) {
		run := interlockedRun(w)
		run.CompletionContract = nil // frozen revision but NULL contract
		run.MilestonesFrozen = frozenJSON(t, "m1", "m2")
		fs := &fakeStore{runOwned: run}
		svc := New(fs, newBox(t), testParams())
		res, _ := svc.RequestCompletionPermit(context.Background(), w, run.ID, base)
		if res.Granted || res.DenyReason != CompletionDenyContractNotFrozen {
			t.Fatalf("want contract_not_frozen denial, got %+v", res)
		}
		if len(fs.recordedAttempts) != 0 {
			t.Fatal("a contract_not_frozen denial must NOT record a completion attempt")
		}
	})

	t.Run("missing milestones records an attempt", func(t *testing.T) {
		run := interlockedRun(w)
		run.CompletionContract = contractJSON(t, "m1", "m2")
		run.MilestonesCompleted = idsJSON(t, "m1")
		fs := &fakeStore{runOwned: run, recordAttemptCount: 1}
		svc := New(fs, newBox(t), testParams())
		res, err := svc.RequestCompletionPermit(context.Background(), w, run.ID, base)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if res.Granted || res.DenyReason != CompletionDenyMissingMilestones {
			t.Fatalf("want missing_milestones denial, got %+v", res)
		}
		if len(res.Unmet) != 1 || res.Unmet[0] != "m2" {
			t.Fatalf("unmet = %v, want [m2]", res.Unmet)
		}
		if len(fs.recordedAttempts) != 1 {
			t.Fatalf("missing_milestones must record exactly one attempt; got %d", len(fs.recordedAttempts))
		}
		if fs.recordedAttempts[0].Head.String != base.Head {
			t.Fatalf("recorded attempt head = %q, want %q", fs.recordedAttempts[0].Head.String, base.Head)
		}
		if fs.upsertPermitParams != nil {
			t.Fatal("a denied permit must NOT be upserted")
		}
	})

	t.Run("granted issues a permit bound to the identity", func(t *testing.T) {
		run := interlockedRun(w)
		run.CompletionContract = contractJSON(t, "m1")
		run.MilestonesCompleted = idsJSON(t, "m1")
		fs := &fakeStore{runOwned: run}
		svc := New(fs, newBox(t), testParams())
		res, err := svc.RequestCompletionPermit(context.Background(), w, run.ID, base)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if !res.Granted || res.Permit == nil {
			t.Fatalf("want granted permit, got %+v", res)
		}
		if fs.upsertPermitParams == nil {
			t.Fatal("grant must call UpsertCompletionPermit")
		}
		if fs.upsertPermitParams.Head != base.Head || fs.upsertPermitParams.Branch != base.Branch {
			t.Fatalf("permit bound to %+v, want head=%q branch=%q", fs.upsertPermitParams, base.Head, base.Branch)
		}
		if fs.upsertPermitParams.ContractRevision != 1 {
			t.Fatalf("permit revision = %d, want the run's frozen revision 1", fs.upsertPermitParams.ContractRevision)
		}
		if !fs.upsertPermitParams.IssuedByWorkerID.Valid {
			t.Fatal("permit must carry the issuing worker (claim fence)")
		}
	})
}

// TestRecordCompletionAttemptEndpoint pins the same-lead nudge endpoint's fence + recompute.
func TestRecordCompletionAttemptEndpoint(t *testing.T) {
	w := worker()

	t.Run("stale claim -> sentinel", func(t *testing.T) {
		run := interlockedRun(w)
		run.Status = "queued"
		fs := &fakeStore{runOwned: run}
		svc := New(fs, newBox(t), testParams())
		if _, err := svc.RecordCompletionAttempt(context.Background(), w, run.ID, CompletionAttemptRequest{Head: "h"}); !errors.Is(err, ErrCompletionStaleClaim) {
			t.Fatalf("want ErrCompletionStaleClaim, got %v", err)
		}
	})

	t.Run("legacy -> not interlocked sentinel", func(t *testing.T) {
		run := interlockedRun(w)
		run.CompletionContractVersion = pgtype.Int4{}
		fs := &fakeStore{runOwned: run}
		svc := New(fs, newBox(t), testParams())
		if _, err := svc.RecordCompletionAttempt(context.Background(), w, run.ID, CompletionAttemptRequest{Head: "h"}); !errors.Is(err, ErrCompletionNotInterlocked) {
			t.Fatalf("want ErrCompletionNotInterlocked, got %v", err)
		}
	})

	t.Run("records server-recomputed unmet + count", func(t *testing.T) {
		run := interlockedRun(w)
		run.CompletionContract = contractJSON(t, "m1", "m2", "m3")
		run.MilestonesCompleted = idsJSON(t, "m1")
		fs := &fakeStore{runOwned: run, recordAttemptCount: 4}
		svc := New(fs, newBox(t), testParams())
		res, err := svc.RecordCompletionAttempt(context.Background(), w, run.ID, CompletionAttemptRequest{Head: "h", WorktreeFingerprint: "wf"})
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if res.AttemptCount != 4 {
			t.Fatalf("attempt_count = %d, want 4", res.AttemptCount)
		}
		if len(res.Unmet) != 2 || res.Unmet[0] != "m2" || res.Unmet[1] != "m3" {
			t.Fatalf("unmet = %v, want [m2 m3]", res.Unmet)
		}
		if len(fs.recordedAttempts) != 1 || fs.recordedAttempts[0].WorktreeFingerprint.String != "wf" {
			t.Fatalf("attempt not recorded with fingerprint; got %+v", fs.recordedAttempts)
		}
	})
}
