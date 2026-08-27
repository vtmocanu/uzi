package workersvc

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestClaimGateSkipsDrainingWorker: a cordoned worker (draining_since set) reports
// idle and never reaches ClaimRun — like the vault gate, it refuses only NEW claims
// (PRD #422 M3, Decision 6/7). The worker keeps heartbeating and finishing its
// in-flight runs; nothing here fails the run.
func TestClaimGateSkipsDrainingWorker(t *testing.T) {
	// claimErr set so that IF the gate wrongly let the claim through, ClaimRun would be
	// reached and return an empty queue — making the claimParams assertion, not the nil
	// payload, the thing that proves the gate fired.
	fs := &fakeStore{claimErr: pgx.ErrNoRows}
	svc := New(fs, newBox(t), testParams())

	wkr := store.Worker{
		ID:            uuid.New(),
		UserID:        uuid.New(),
		DrainingSince: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}
	payload, err := svc.Claim(context.Background(), wkr)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if payload != nil {
		t.Fatal("expected idle (nil payload) while the worker is draining")
	}
	if fs.claimParams != nil {
		t.Fatal("ClaimRun must NOT be called when the worker is draining/cordoned")
	}
}

// TestClaimNonDrainingWorkerProceeds: a worker with a NULL draining_since is not
// cordoned, so Claim proceeds past the drain gate and reaches ClaimRun. The queue is
// empty here (idle), but claimParams proves the gate let the claim through — the
// mirror assertion to the draining case, so the two together pin the gate's effect.
func TestClaimNonDrainingWorkerProceeds(t *testing.T) {
	fs := &fakeStore{claimErr: pgx.ErrNoRows}
	svc := New(fs, newBox(t), testParams())

	// worker() constructs a store.Worker with a zero-value DrainingSince (Valid=false).
	payload, err := svc.Claim(context.Background(), worker())
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if payload != nil {
		t.Fatal("expected idle (empty queue) for a non-draining worker")
	}
	if fs.claimParams == nil {
		t.Fatal("ClaimRun MUST be reached for a non-draining worker (the drain gate must not fire)")
	}
}
