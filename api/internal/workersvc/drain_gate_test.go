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

// TestClaimGateDrainingWorkerReachesClaimRunScoped: PRD #1030 M2 narrows the PRD #422
// M3 drain gate. A cordoned worker (draining_since set) MUST still reach ClaimRun so it
// can re-claim its OWN promoted run through an image roll (the run parked while its owner
// was cordoned must resume in place on the same worker/PVC, not be stolen cold by a peer —
// run #1009). The old early return that made a draining worker claim nothing has been
// replaced by threading ClaimantDraining=true into ClaimRun, whose
// `NOT @claimant_draining OR r.worker_id = @worker_id` clause scopes a draining claimant
// to its own runs only (PRD #422 D7: "a draining worker claims nothing new"). Here the
// queue is empty (claimErr=ErrNoRows), so the payload is nil (idle) either way — the
// claimParams assertions are what prove the claim reached ClaimRun AND carried the
// draining scope flag.
func TestClaimGateDrainingWorkerReachesClaimRunScoped(t *testing.T) {
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
		t.Fatal("expected idle (nil payload) for an empty queue")
	}
	if fs.claimParams == nil {
		t.Fatal("PRD #1030 M2: a draining worker MUST reach ClaimRun (to re-claim its own promoted run through a roll)")
	}
	if !fs.claimParams.ClaimantDraining {
		t.Fatal("a draining worker's ClaimRun call must set ClaimantDraining=true so the SQL scopes it to its own runs")
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
	if fs.claimParams.ClaimantDraining {
		t.Fatal("a non-draining worker's ClaimRun call must set ClaimantDraining=false (a no-op scope, PRD #1030 M2)")
	}
}
