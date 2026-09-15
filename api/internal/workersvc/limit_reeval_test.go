package workersvc

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// reevalRow builds one ListLimitWaitReeval worklist row: a run STILL parked in
// limit_wait (retry_not_before an hour out) that recorded a dead credential. kind is
// issue; workerBindMode drives the LEFT-JOINed worker bind mode ("" ⇒ NULL, i.e. an
// absent worker ⇒ effectiveNextClaimMode unknown). The parked runs share parkNow's
// clock via the fixture's svc.now.
func reevalRow(id, owner uuid.UUID, workerBindMode string, dead uuid.UUID) store.ListLimitWaitReevalRow {
	return store.ListLimitWaitReevalRow{
		ID:                id,
		UserID:            owner,
		Kind:              runkind.Issue,
		WorkerID:          pgtype.UUID{Bytes: uuid.New(), Valid: true},
		WorkerBindMode:    pgtype.Text{String: workerBindMode, Valid: workerBindMode != ""},
		LimitDeadSecretID: pgtype.UUID{Bytes: dead, Valid: true},
		RetryNotBefore:    pgtype.Timestamptz{Time: parkNow.Add(time.Hour), Valid: true},
	}
}

func reevalSvc(t *testing.T, fs *fakeStore) *Service {
	t.Helper()
	// autoParams, not testParams: the D8 floor is autoselect.NextAvailable, which needs a
	// REAL policy (testParams leaves MaxStaleness 0, so every candidate classifies stale
	// and nothing is ever spendable — the pass would skip every run for the wrong reason).
	svc := New(fs, newBox(t), autoParams())
	svc.now = func() time.Time { return parkNow }
	svc.SetBroadcaster(&parkBroadcaster{})
	return svc
}

func lowered(fs *fakeStore, id uuid.UUID) bool {
	for _, l := range fs.loweredLimitWait {
		if l.ID == id {
			return true
		}
	}
	return false
}

// 🔴 TestSweepReEvaluatesParkedLimitWaitOnePerOwner is the D8 duration-time pass (PRD
// #1247 M3): two owners each with two parked `auto` runs and a now-eligible pooled
// alternative. ONE Sweep lowers exactly the OLDEST run of each owner (never both), and
// the second Sweep takes each owner's remaining run — the anti-stampede stagger, since
// this pass bypasses the park-time jitter. A third owner's PINNED parked run is never
// lowered, even though its pool has an eligible alternative: promoting a non-auto resume
// would only re-park.
//
// MUTATION THIS CATCHES on two seams:
//   - drop the one-per-owner `seen` guard (or mark seen on consideration rather than on
//     the lowering) → both of an owner's runs lower in one tick, LimitReevaluated jumps.
//   - drop the `effectiveNextClaimMode(...) == BindModeAuto` filter → the pinned run
//     lowers and the "pinned is never lowered" assertion reddens.
func TestSweepReEvaluatesParkedLimitWaitOnePerOwner(t *testing.T) {
	owner1, owner2, owner3 := uuid.New(), uuid.New(), uuid.New()
	dead1, dead2, dead3 := uuid.New(), uuid.New(), uuid.New()
	alt1, alt2, alt3 := uuid.New(), uuid.New(), uuid.New()

	// Oldest first, exactly as ListLimitWaitReeval (status_since ASC) returns.
	a1, b1 := uuid.New(), uuid.New() // owner1: a1 older than b1
	c2, d2 := uuid.New(), uuid.New() // owner2: c2 older than d2
	p3 := uuid.New()                 // owner3: a single PINNED parked run

	fs := &fakeStore{
		limitWaitReeval: []store.ListLimitWaitReevalRow{
			reevalRow(a1, owner1, BindModeAuto, dead1),
			reevalRow(c2, owner2, BindModeAuto, dead2),
			reevalRow(p3, owner3, BindModePinned, dead3),
			reevalRow(b1, owner1, BindModeAuto, dead1),
			reevalRow(d2, owner2, BindModeAuto, dead2),
		},
		autoCandidatesByUser: map[uuid.UUID][]store.ListAutoSelectCandidatesRow{
			owner1: {candRow(dead1, "dead1", true, 90, time.Minute, 0), candRow(alt1, "alt1", true, 90, time.Minute, 0)},
			owner2: {candRow(dead2, "dead2", true, 90, time.Minute, 0), candRow(alt2, "alt2", true, 90, time.Minute, 0)},
			owner3: {candRow(dead3, "dead3", true, 90, time.Minute, 0), candRow(alt3, "alt3", true, 90, time.Minute, 0)},
		},
	}
	svc := reevalSvc(t, fs)

	res1, err := svc.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep 1: %v", err)
	}
	if res1.LimitReevaluated != 2 {
		t.Fatalf("Sweep 1 LimitReevaluated = %d, want 2 — one lowering per owner per tick", res1.LimitReevaluated)
	}
	if !lowered(fs, a1) || !lowered(fs, c2) {
		t.Fatalf("Sweep 1 lowered %v, want the OLDEST run of each owner (%s, %s)", fs.loweredLimitWait, a1, c2)
	}
	if lowered(fs, b1) || lowered(fs, d2) {
		t.Fatalf("Sweep 1 lowered a SECOND run of an owner (%v); the stagger caps it at one per owner per tick", fs.loweredLimitWait)
	}
	if lowered(fs, p3) {
		t.Fatal("Sweep 1 lowered the PINNED run p3; a non-auto next claim would only re-park, so it must be skipped")
	}

	res2, err := svc.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep 2: %v", err)
	}
	if res2.LimitReevaluated != 2 {
		t.Fatalf("Sweep 2 LimitReevaluated = %d, want 2 — each owner's remaining run this tick", res2.LimitReevaluated)
	}
	if !lowered(fs, b1) || !lowered(fs, d2) {
		t.Fatalf("Sweep 2 lowered %v, want each owner's remaining run (%s, %s)", fs.loweredLimitWait, b1, d2)
	}
	if lowered(fs, p3) {
		t.Fatal("the pinned run p3 was lowered on the second tick; it must NEVER be lowered")
	}
	// The auto-eligible LEFT JOIN clock the pass ran on is the sweep's own now.
	if len(fs.limitWaitReevalAt) != 2 || !fs.limitWaitReevalAt[0].Time.Equal(parkNow) {
		t.Fatalf("reeval worklist clock = %v, want two reads at the sweep's now %v", fs.limitWaitReevalAt, parkNow)
	}
}

// TestSweepReEvaluateSkipsWhenNoSpendableAlternative pins the two "do not lower" pass
// cases that are NOT about mode: an `auto` run whose ONLY pooled token is its own
// still-excluded dead credential (NextAvailable refuses the dead token voting for its own
// replacement), and an `auto` run whose pool is genuinely empty. Both must stay parked.
// An `unknown`-worker run (no recorded/loadable worker) is skipped too, the safe way.
func TestSweepReEvaluateSkipsWhenNoSpendableAlternative(t *testing.T) {
	soleDeadOwner, emptyOwner, unknownOwner := uuid.New(), uuid.New(), uuid.New()
	dead := uuid.New()

	soleDead := uuid.New()
	empty := uuid.New()
	unknownRun := uuid.New()

	fs := &fakeStore{
		limitWaitReeval: []store.ListLimitWaitReevalRow{
			reevalRow(soleDead, soleDeadOwner, BindModeAuto, dead),
			reevalRow(empty, emptyOwner, BindModeAuto, dead),
			reevalRow(unknownRun, unknownOwner, "", dead), // NULL worker bind mode ⇒ unknown
		},
		autoCandidatesByUser: map[uuid.UUID][]store.ListAutoSelectCandidatesRow{
			// The sole pooled token IS the dead credential — NextAvailable excludes it.
			soleDeadOwner: {candRow(dead, "dead", true, 90, time.Minute, 0)},
			// A genuinely empty pool.
			emptyOwner: {},
			// The unknown-worker owner even HAS an eligible alternative; it must still be
			// skipped because its next claim's mode cannot be predicted.
			unknownOwner: {candRow(dead, "dead", true, 90, time.Minute, 0), candRow(uuid.New(), "alt", true, 90, time.Minute, 0)},
		},
	}
	svc := reevalSvc(t, fs)

	res, err := svc.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if res.LimitReevaluated != 0 {
		t.Fatalf("LimitReevaluated = %d, want 0 — none of these owners has a spendable auto alternative", res.LimitReevaluated)
	}
	if len(fs.loweredLimitWait) != 0 {
		t.Fatalf("lowered %v, want none — no run here should have its park lowered", fs.loweredLimitWait)
	}
}

// TestSweepReEvaluateListErrorFailsPass: a ListLimitWaitReeval error, like the
// limit-promote and pool-resume list reads, fails the whole sweep rather than being
// swallowed — a torn worklist read is not "nothing to re-evaluate".
func TestSweepReEvaluateListErrorFailsPass(t *testing.T) {
	fs := &fakeStore{limitWaitReevalErr: context.DeadlineExceeded}
	svc := reevalSvc(t, fs)
	if _, err := svc.Sweep(context.Background()); err == nil {
		t.Fatal("Sweep returned nil, want the ListLimitWaitReeval error wrapped and returned")
	}
}

// TestSweepReEvaluateSkipsCandidateReadFailure: a per-owner candidate-query error is
// logged and skipped (best-effort), never failing the whole sweep — mirroring the
// pool-resume pass. The run stays parked for the next tick.
func TestSweepReEvaluateSkipsCandidateReadFailure(t *testing.T) {
	owner := uuid.New()
	fs := &fakeStore{
		limitWaitReeval:   []store.ListLimitWaitReevalRow{reevalRow(uuid.New(), owner, BindModeAuto, uuid.New())},
		autoCandidatesErr: context.DeadlineExceeded,
	}
	svc := reevalSvc(t, fs)
	res, err := svc.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep must not fail when a single owner's candidate read fails: %v", err)
	}
	if res.LimitReevaluated != 0 || len(fs.loweredLimitWait) != 0 {
		t.Fatalf("reevaluated=%d lowered=%d, want 0/0 after a candidate read failure", res.LimitReevaluated, len(fs.loweredLimitWait))
	}
}
