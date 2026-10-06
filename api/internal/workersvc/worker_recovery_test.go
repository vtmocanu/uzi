package workersvc

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestWorkerRecoveryLedgerEvaluation(t *testing.T) {
	svc := &Service{}
	if applied, err := svc.ReplaceWorkerActiveRuns(context.Background(), nil, store.Worker{}, nil, snapshotModeHeartbeat); applied || err != nil {
		t.Fatalf("absent snapshot queried or applied: %v %v", applied, err)
	}
	// Refusal precedes any query, including with otherwise empty input.
	if _, err := svc.applyWorkerActiveSnapshot(context.Background(), nil, store.Worker{}, &ActiveSnapshot{}, snapshotModeHeartbeat, nil, workerRecoveryLockSet{}); err == nil {
		t.Fatal("unevaluated snapshot ledger accepted")
	}
	if _, _, _, err := svc.runFrozenAttested(context.Background(), nil, uuid.Nil, 0, store.FrozenFailWorkerRunsOverCapParams{}, validatedFinalizeResume{}, false, workerRecoveryLockSet{}); err == nil {
		t.Fatal("unevaluated recovery ledger accepted")
	}
	empty, err := workerRecoveryLocksFromRows(nil)
	if err != nil {
		t.Fatal(err)
	}
	frozen, parents, err := empty.parameters()
	if err != nil || string(frozen) != "[]" || parents == nil || len(parents) != 0 {
		t.Fatalf("evaluated empty ledger: json=%s parents=%v err=%v", frozen, parents, err)
	}
	a, b := uuid.New(), uuid.New()
	rows := []store.LockWorkerRecoveryParentsRow{
		{ID: uuid.New(), LockedParentIds: []uuid.UUID{a, b}},
		{ID: uuid.New(), LockedParentIds: []uuid.UUID{b}},
		{ID: uuid.New(), ParentLeadID: pgtype.UUID{Bytes: uuid.New(), Valid: true}},
	}
	ledger, err := workerRecoveryLocksFromRows(rows)
	if err != nil {
		t.Fatal(err)
	}
	_, parents, err = ledger.parameters()
	if err != nil || len(parents) != 2 {
		t.Fatalf("actual lock union: %v %v", parents, err)
	}
	parents[0] = uuid.Nil
	_, again, _ := ledger.parameters()
	if again[0] == uuid.Nil {
		t.Fatal("caller mutated ledger")
	}
}

func TestWorkerRecoverySnapshotValidation(t *testing.T) {
	p := testParams()
	p.ActiveSnapshotMaxEntries = 4
	p.WorkerOutboxMaxPending = 1
	w := store.Worker{ID: uuid.New(), SnapshotRegisterNonce: pgtype.Text{String: "nonce", Valid: true}, SnapshotEpoch: 2, MaxConcurrentRuns: pgtype.Int4{Int32: 0, Valid: true}}
	good := ActiveSnapshot{SnapshotEpoch: 3, RegisterNonce: "nonce"}
	cases := []struct {
		name   string
		modify func(*ActiveSnapshot)
		valid  bool
	}{
		{"empty", func(s *ActiveSnapshot) {}, true},
		{"stale", func(s *ActiveSnapshot) { s.SnapshotEpoch = 2 }, false},
		{"nonce", func(s *ActiveSnapshot) { s.RegisterNonce = "wrong" }, false},
		{"malformed", func(s *ActiveSnapshot) { s.Active = []ActiveRunEntry{{RunID: "bad"}} }, false},
		{"reported future generation", func(s *ActiveSnapshot) { s.Active = []ActiveRunEntry{entry(uuid.New(), 99, "running", false)} }, true},
		{"negative generation", func(s *ActiveSnapshot) { s.Active = []ActiveRunEntry{entry(uuid.New(), -1, "running", false)} }, false},
		{"duplicate", func(s *ActiveSnapshot) {
			e := entry(uuid.New(), 1, "running", false)
			s.Active = []ActiveRunEntry{e, e}
		}, false},
		{"phase", func(s *ActiveSnapshot) { s.Active = []ActiveRunEntry{entry(uuid.New(), 1, "bogus", false)} }, false},
		{"live cap", func(s *ActiveSnapshot) {
			for i := 0; i < 3; i++ {
				s.Active = append(s.Active, entry(uuid.New(), 1, "running", false))
			}
		}, false},
		{"pending cap", func(s *ActiveSnapshot) {
			for i := 0; i < 2; i++ {
				s.Active = append(s.Active, entry(uuid.New(), 1, "running", true))
			}
		}, false},
		{"absolute cap", func(s *ActiveSnapshot) {
			for i := 0; i < 5; i++ {
				s.Active = append(s.Active, entry(uuid.New(), 1, "running", false))
			}
		}, false},
	}
	svc := &Service{p: p}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap := good
			tc.modify(&snap)
			entries, reason := normalizeActiveSnapshot(p, w, &snap, snapshotModeHeartbeat)
			if (reason == "") != tc.valid {
				t.Fatalf("valid=%v reason=%s", tc.valid, reason)
			}
			if tc.valid && len(entries) != len(snap.Active) {
				t.Fatal("normalization lost entries")
			}
			_, applied, err := svc.validatedActiveSnapshot(w, &snap, snapshotModeHeartbeat)
			if applied != tc.valid || err != nil {
				t.Fatalf("heartbeat applied=%v err=%v", applied, err)
			}
			_, applied, err = svc.validatedActiveSnapshot(w, &snap, snapshotModeClaim)
			if applied != tc.valid || (!tc.valid && !errors.Is(err, ErrActiveSnapshotInvalid)) {
				t.Fatalf("claim applied=%v err=%v", applied, err)
			}
		})
	}
	// Register exempts nonce/epoch, while retaining shape/cap checks.
	snap := good
	snap.SnapshotEpoch = 0
	snap.RegisterNonce = ""
	if _, reason := normalizeActiveSnapshot(p, w, &snap, snapshotModeRegister); reason != "" {
		t.Fatal(reason)
	}
}
