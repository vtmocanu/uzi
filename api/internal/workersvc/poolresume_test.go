package workersvc

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// eligibleCand is a single AutoEligible candidate row — enough to make
// resumePoolWaitRuns treat the owner's pool as non-empty (its test uses the same
// "at least one AutoEligible" condition autoselect.Floor spends on).
func eligibleCand() []store.ListAutoSelectCandidatesRow {
	return []store.ListAutoSelectCandidatesRow{{UserSecretID: uuid.New(), AutoEligible: true}}
}

// ineligibleCand is a candidate row that is NOT AutoEligible — a pooled-but-opted-out
// (or stale) token, which does not make the pool resumable.
func ineligibleCand() []store.ListAutoSelectCandidatesRow {
	return []store.ListAutoSelectCandidatesRow{{UserSecretID: uuid.New(), AutoEligible: false}}
}

func heldRun(userID uuid.UUID) store.ListPoolWaitRunsRow {
	return store.ListPoolWaitRunsRow{ID: uuid.New(), UserID: userID, StatusSince: pgtype.Timestamptz{Time: parkNow, Valid: true}}
}

// TestSweepResumesPoolWaitForResumableOwner: a held run whose owner now has an
// AutoEligible token is promoted (pool_wait → queued), broadcast, and counted in
// PoolResumed; a held run whose owner has NO eligible token is left held. Both users
// are swept in ONE Sweep to prove the per-user discrimination, not just the happy path.
func TestSweepResumesPoolWaitForResumableOwner(t *testing.T) {
	resumable := uuid.New()
	stuck := uuid.New()
	heldResumable := heldRun(resumable)
	heldStuck := heldRun(stuck)

	fs := &fakeStore{
		poolWaitRuns: []store.ListPoolWaitRunsRow{heldResumable, heldStuck},
		autoCandidatesByUser: map[uuid.UUID][]store.ListAutoSelectCandidatesRow{
			resumable: eligibleCand(),
			stuck:     ineligibleCand(),
		},
	}
	svc := New(fs, newBox(t), testParams())
	svc.now = func() time.Time { return parkNow }
	bc := &parkBroadcaster{}
	svc.SetBroadcaster(bc)

	res, err := svc.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if res.PoolResumed != 1 {
		t.Fatalf("PoolResumed = %d, want 1 (only the owner with an eligible token resumes)", res.PoolResumed)
	}
	if len(fs.promotedPoolWait) != 1 {
		t.Fatalf("PromotePoolWaitRun called %d times, want 1", len(fs.promotedPoolWait))
	}
	if got := fs.promotedPoolWait[0]; got.ID != heldResumable.ID || got.UserID != resumable {
		t.Fatalf("promoted {id=%s user=%s}, want the resumable owner's held run {id=%s user=%s} — "+
			"the promote must be owner-scoped and target the resumable user's run",
			got.ID, got.UserID, heldResumable.ID, resumable)
	}
	if !bc.sawState(heldResumable.ID, "queued") {
		t.Fatalf("the resume of %s was not broadcast as queued; a resumed run would sit invisible "+
			"in the board's In Progress column", heldResumable.ID)
	}
	if bc.sawState(heldStuck.ID, "queued") {
		t.Fatalf("the stuck owner's held run %s was broadcast as resumed, but its pool has no "+
			"eligible token — it must stay held", heldStuck.ID)
	}
}

// TestSweepPoolResumeIsOnePerUserPerTick: two held runs for ONE resumable owner are
// resumed one at a time — the first Sweep promotes only the oldest, the second promotes
// the next. This is the anti-stampede stagger: promoting both onto the single token at
// once would thundering-herd it.
func TestSweepPoolResumeIsOnePerUserPerTick(t *testing.T) {
	owner := uuid.New()
	// oldest first (status_since ASC), exactly as ListPoolWaitRuns returns.
	older := store.ListPoolWaitRunsRow{ID: uuid.New(), UserID: owner, StatusSince: pgtype.Timestamptz{Time: parkNow.Add(-time.Minute), Valid: true}}
	newer := store.ListPoolWaitRunsRow{ID: uuid.New(), UserID: owner, StatusSince: pgtype.Timestamptz{Time: parkNow, Valid: true}}

	fs := &fakeStore{
		poolWaitRuns: []store.ListPoolWaitRunsRow{older, newer},
		autoCandidatesByUser: map[uuid.UUID][]store.ListAutoSelectCandidatesRow{
			owner: eligibleCand(),
		},
	}
	svc := New(fs, newBox(t), testParams())
	svc.now = func() time.Time { return parkNow }
	svc.SetBroadcaster(&parkBroadcaster{})

	res1, err := svc.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep 1: %v", err)
	}
	if res1.PoolResumed != 1 {
		t.Fatalf("Sweep 1 PoolResumed = %d, want 1 — at most one held run per owner per tick", res1.PoolResumed)
	}
	if fs.promotedPoolWait[0].ID != older.ID {
		t.Fatalf("Sweep 1 promoted %s, want the OLDEST held run %s (longest-waiting first)", fs.promotedPoolWait[0].ID, older.ID)
	}

	// The fake drops a resumed run from its worklist, so the second Sweep sees only the
	// remaining held run — the live query would too, since the first is no longer pool_wait.
	res2, err := svc.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep 2: %v", err)
	}
	if res2.PoolResumed != 1 {
		t.Fatalf("Sweep 2 PoolResumed = %d, want 1", res2.PoolResumed)
	}
	if len(fs.promotedPoolWait) != 2 {
		t.Fatalf("total promotes = %d, want 2 across two Sweeps (one per tick)", len(fs.promotedPoolWait))
	}
	if fs.promotedPoolWait[1].ID != newer.ID {
		t.Fatalf("Sweep 2 promoted %s, want the remaining held run %s", fs.promotedPoolWait[1].ID, newer.ID)
	}
}

// TestSweepPoolResumeSkipsCandidateReadFailure: a per-user candidate-query error is
// logged and skipped, never failing the whole sweep — the run stays held for the next
// tick and other users are still swept.
func TestSweepPoolResumeSkipsCandidateReadFailure(t *testing.T) {
	owner := uuid.New()
	fs := &fakeStore{
		poolWaitRuns:      []store.ListPoolWaitRunsRow{heldRun(owner)},
		autoCandidatesErr: context.DeadlineExceeded, // the per-user read fails
	}
	svc := New(fs, newBox(t), testParams())
	svc.now = func() time.Time { return parkNow }
	svc.SetBroadcaster(&parkBroadcaster{})

	res, err := svc.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep must not fail when a single user's candidate read fails: %v", err)
	}
	if res.PoolResumed != 0 {
		t.Fatalf("PoolResumed = %d, want 0 (the read failed, so nothing resumed)", res.PoolResumed)
	}
	if len(fs.promotedPoolWait) != 0 {
		t.Fatalf("PromotePoolWaitRun ran %d times after a candidate read failure, want 0", len(fs.promotedPoolWait))
	}
}

// TestSweepPoolResumeListErrorFailsSweep: a ListPoolWaitRuns error, unlike a per-user
// candidate read, fails the whole pass — it mirrors the limit-promote's own read.
func TestSweepPoolResumeListErrorFailsSweep(t *testing.T) {
	fs := &fakeStore{poolWaitRunsErr: context.DeadlineExceeded}
	svc := New(fs, newBox(t), testParams())
	svc.now = func() time.Time { return parkNow }
	svc.SetBroadcaster(&parkBroadcaster{})

	if _, err := svc.Sweep(context.Background()); err == nil {
		t.Fatal("Sweep returned nil, want the ListPoolWaitRuns error wrapped and returned")
	}
}
