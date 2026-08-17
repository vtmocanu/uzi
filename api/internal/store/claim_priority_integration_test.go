package store_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Priority-aware ClaimRun (PRD #320 M2 / D3, D4) against a REAL Postgres — the
// ORDER BY behaviour the fake-store unit tests cannot cover because they never run
// the SQL: the fn_run_priority rank slotted BETWEEN resume affinity and FIFO
// (interactive beats an earlier background run, an expedited run beats everything),
// the D4 age fail-open (a demoted run older than @background_grace_cutoff collapses
// to normal priority so background work never starves), resume affinity still
// winning over priority, and FIFO holding within one level. A regression subtest
// mirrors a #216 spread assertion to prove the WHERE eligibility/spread is
// unchanged by the ORDER BY edit.
//
// The demoted kind used here is judge (repo-less, keyed to a target_run_id), which
// the D5 demotion set covers alongside self_improve; the ordering behaviour is the
// same for both. judge is preferred over self_improve in these tests because
// self_improve carries an instance-wide singleton index (uq_runs_one_active_self_improve),
// so two non-terminal self_improve rows cannot coexist across the DB the tests
// share, whereas judge is unique only per target_run_id. Every test is skipped
// unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (reuses the
// fleetFixture preamble in claim_fleet_placement_integration_test.go).

// insertPRun inserts a queued run under the fixture's user with an explicit kind,
// manual priority override (nil ⇒ NULL, use kind default), owning worker (nil ⇒
// unowned), and created_at (updated_at is pinned to the same instant). It returns
// the run id. self_improve/issue both need repo_id + issue_iid, which the fixture
// supplies.
func (fx *fleetFixture) insertPRun(kind string, priority *int32, worker *uuid.UUID, createdAt time.Time) uuid.UUID {
	fx.t.Helper()
	id := uuid.New()
	var wid interface{}
	if worker != nil {
		wid = *worker
	}
	mustExec(fx.ctx, fx.t, fx.pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind, priority, worker_id, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, 't', 'd', 'queued', $5, $6, $7, $8, $8)`,
		id, fx.userID, fx.repoID, fx.nextIID(), kind, priority, wid, createdAt)
	return id
}

// claimWithGrace claims as workerID, threading @background_grace_cutoff (D4) so the
// tests control which demoted runs read as stale. Affinity/spread/heartbeat cutoffs
// match the fixture's defaults; a run older than @background_grace_cutoff fails open
// to normal priority.
func (fx *fleetFixture) claimWithGrace(workerID uuid.UUID, backgroundGraceCutoff time.Time) (store.Run, error) {
	return fx.q.ClaimRun(fx.ctx, store.ClaimRunParams{
		WorkerID:              pgtype.UUID{Bytes: workerID, Valid: true},
		UserID:                fx.userID,
		AffinityCutoff:        pgtype.Timestamptz{Time: time.Now().Add(-2 * time.Minute), Valid: true},
		SpreadCutoff:          pgtype.Timestamptz{Time: time.Now().Add(-9 * time.Second), Valid: true},
		BackgroundGraceCutoff: pgtype.Timestamptz{Time: backgroundGraceCutoff, Valid: true},
		HeartbeatCutoff:       pgtype.Timestamptz{Time: time.Now().Add(-45 * time.Second), Valid: true},
		IsDockerWorker:        false,
		DockerRepoAllowlist:   nil,
	})
}

// insertJudge inserts a queued judge run (repo-less, per runs_kind_shape) reviewing
// target, with an explicit manual priority override and created_at. judge is a
// demoted kind (D5), so with a nil override and a non-stale created_at it ranks 0.
func (fx *fleetFixture) insertJudge(priority *int32, target uuid.UUID, createdAt time.Time) uuid.UUID {
	fx.t.Helper()
	id := uuid.New()
	mustExec(fx.ctx, fx.t, fx.pool,
		`INSERT INTO runs (id, user_id, issue_title, issue_description, status, kind, priority, target_run_id, created_at, updated_at)
		 VALUES ($1, $2, 't', 'd', 'queued', 'judge', $3, $4, $5, $5)`,
		id, fx.userID, priority, target, createdAt)
	return id
}

func prio(n int32) *int32 { return &n }

// D3: an interactive issue run created AFTER an earlier background judge run is
// claimed FIRST — the priority rank (1 > 0) overrides FIFO, which would have ordered
// the judge first. Single worker, so the spread never defers.
func TestClaimRunInteractiveBeatsBackgroundLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	w := fx.worker("A", capOf(2), false)
	now := time.Now()
	// issue is newer but rank 1 (interactive); it also serves as the judge's target.
	interactive := fx.insertPRun("issue", nil, nil, now.Add(-1*time.Minute))
	// judge is older (would win under pure FIFO) but demoted to rank 0.
	bg := fx.insertJudge(nil, interactive, now.Add(-2*time.Minute))
	// Cutoff is well before both, so neither is stale: the judge stays demoted.
	c, err := fx.claimWithGrace(w, now.Add(-15*time.Minute))
	if err != nil {
		t.Fatalf("claim must succeed: %v", err)
	}
	if c.ID != interactive {
		t.Fatalf("interactive issue must be claimed before the earlier background run; claimed %s, want %s (bg=%s)", c.ID, interactive, bg)
	}
}

// D6/D2: an expedited run (priority = 2) created LAST is claimed before an earlier
// normal issue run — the override outranks the kind default.
func TestClaimRunExpediteBeatsAllLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	w := fx.worker("A", capOf(2), false)
	now := time.Now()
	normal := fx.insertPRun("issue", nil, nil, now.Add(-5*time.Minute))
	expedited := fx.insertPRun("issue", prio(2), nil, now.Add(-1*time.Minute))
	c, err := fx.claimWithGrace(w, now.Add(-15*time.Minute))
	if err != nil {
		t.Fatalf("claim must succeed: %v", err)
	}
	if c.ID != expedited {
		t.Fatalf("expedited run must be claimed first; claimed %s, want %s (normal=%s)", c.ID, expedited, normal)
	}
}

// D4 fail-open: a demoted judge run whose created_at is older than
// @background_grace_cutoff reads as stale, so fn_run_priority returns NORMAL (rank
// 1). Against a fresher normal issue run (also rank 1, created LATER), FIFO then
// claims the older-but-restored judge first — proving it is no longer deprioritized
// below a normal run. Without the fail-open it would rank 0 and the issue would be
// claimed first.
func TestClaimRunFailOpenAfterGraceLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	w := fx.worker("A", capOf(2), false)
	now := time.Now()
	cutoff := now.Add(-15 * time.Minute)
	// normal issue created AFTER the cutoff ⇒ not stale ⇒ rank 1; it is LATER than the
	// judge and also serves as the judge's target.
	fresh := fx.insertPRun("issue", nil, nil, now.Add(-10*time.Minute))
	// judge created WELL BEFORE the cutoff ⇒ stale ⇒ fail-open to rank 1, and being
	// earlier it must win on FIFO.
	restored := fx.insertJudge(nil, fresh, now.Add(-30*time.Minute))
	c, err := fx.claimWithGrace(w, cutoff)
	if err != nil {
		t.Fatalf("claim must succeed: %v", err)
	}
	if c.ID != restored {
		t.Fatalf("a demoted run past the grace fails open to normal and (being earlier) wins on FIFO; claimed %s, want %s (fresh=%s)", c.ID, restored, fresh)
	}
}

// D3: resume affinity is the FIRST ORDER BY term, so a worker reclaiming ITS OWN
// re-queued run wins over a strictly higher-priority (expedited) run owned by no
// one. Single worker, so the spread never defers.
func TestClaimRunAffinityStillWinsOverPriorityLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	w := fx.worker("A", capOf(2), false)
	now := time.Now()
	// A's own re-queued normal run.
	mine := fx.insertPRun("issue", nil, &w, now.Add(-1*time.Minute))
	// An expedited unowned run — higher priority rank (2) but not owned by A.
	expedited := fx.insertPRun("issue", prio(2), nil, now.Add(-2*time.Minute))
	c, err := fx.claimWithGrace(w, now.Add(-15*time.Minute))
	if err != nil {
		t.Fatalf("claim must succeed: %v", err)
	}
	if c.ID != mine {
		t.Fatalf("resume affinity must beat a higher-priority unowned run; claimed %s, want %s (expedited=%s)", c.ID, mine, expedited)
	}
}

// D3: within one priority level, FIFO by created_at is preserved — the earlier of
// two equal-priority issue runs is claimed first.
func TestClaimRunFIFOWithinLevelLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	w := fx.worker("A", capOf(2), false)
	now := time.Now()
	earlier := fx.insertPRun("issue", nil, nil, now.Add(-5*time.Minute))
	later := fx.insertPRun("issue", nil, nil, now.Add(-1*time.Minute))
	c, err := fx.claimWithGrace(w, now.Add(-15*time.Minute))
	if err != nil {
		t.Fatalf("claim must succeed: %v", err)
	}
	if c.ID != earlier {
		t.Fatalf("FIFO within a priority level: earlier run must be claimed first; claimed %s, want %s (later=%s)", c.ID, earlier, later)
	}
}

// Regression (PRD #216): the ORDER BY edit must not change WHERE eligibility or the
// fleet-aware spread. Two idle cap-2 workers, two fresh queued runs: A claims one,
// then DEFERS the second to still-idle B (strictly less loaded), and B claims it —
// the exact (a1) spread behaviour, now driven through claimWithGrace so the new
// @background_grace_cutoff param is supplied.
func TestClaimRunSpreadUnchangedByPriorityLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	a := fx.worker("A", capOf(2), false)
	b := fx.worker("B", capOf(2), false)
	now := time.Now()
	cutoff := now.Add(-15 * time.Minute)
	r1 := fx.insertPRun("issue", nil, nil, now)
	r2 := fx.insertPRun("issue", nil, nil, now)

	c1, err := fx.claimWithGrace(a, cutoff)
	if err != nil {
		t.Fatalf("A's first claim must succeed (both workers idle): %v", err)
	}
	// A now holds 1; idle peer B is strictly less loaded and has a free slot, so A
	// must DEFER rather than pile a second run on itself — spread unchanged.
	if _, err := fx.claimWithGrace(a, cutoff); err != pgx.ErrNoRows {
		t.Fatalf("A holding 1 must defer to idle peer B; got %v, want pgx.ErrNoRows", err)
	}
	c2, err := fx.claimWithGrace(b, cutoff)
	if err != nil {
		t.Fatalf("B must claim the deferred run: %v", err)
	}
	if c1.ID == c2.ID {
		t.Fatalf("A and B claimed the same run %s", c1.ID)
	}
	got := map[uuid.UUID]bool{c1.ID: true, c2.ID: true}
	if !got[r1] || !got[r2] {
		t.Fatalf("claims %s,%s do not cover both runs %s,%s", c1.ID, c2.ID, r1, r2)
	}
}
