package workersvc

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func retainedCapacityReport(gen int64) StateRequest {
	req := retainedReport(gen)
	req.RetainedCapacityOnly = boolPtr(true)
	return req
}

func (e interlockLiveDB) capacityParkService(t *testing.T, maxParks int) *Service {
	t.Helper()
	p := testParams()
	p.RunRetainedRecoveryMaxParks = maxParks
	svc := New(e.q, newBox(t), p)
	svc.SetTxBeginner(e.pool)
	svc.SetBackground(func(func()) {})
	return svc
}

func (e interlockLiveDB) countHolds(t *testing.T, runID uuid.UUID) (open, total int) {
	t.Helper()
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FILTER (WHERE state='open'), count(*) FROM recovery_custody_holds WHERE run_id=$1`, runID).Scan(&open, &total); err != nil {
		t.Fatal(err)
	}
	return open, total
}

func (e interlockLiveDB) claimFor(t *testing.T, w uuid.UUID, r store.Run) (store.Run, error) {
	t.Helper()
	return e.q.ClaimRun(e.ctx, store.ClaimRunParams{WorkerID: pgconv.UUID(w), UserID: r.UserID,
		IsDockerWorker: true, DockerRepoAllowlist: []uuid.UUID{r.RepoID.Bytes},
		AffinityCutoff: pgconv.Time(time.Now().Add(-time.Hour)), HeartbeatCutoff: pgconv.Time(time.Now().Add(-time.Hour)),
		RecoveryCapable: true, WorkerIdentity: "retained-worker", WorkerProtocolCaps: []string{"recovery_inventory_v1"}})
}

// TestRetainedCapacityOnlyHoldsWithoutCountingLiveDB: a valid marked park takes the at-cap branch
// at every prior count, below or at the cap: no counter increment, NULL retry, owner-held
// recovery_wait with custody, worker and cause kept. A sweeper pass at a far-future clock and a
// claim poll then neither requeue nor claim the run, and open no custody hold.
func TestRetainedCapacityOnlyHoldsWithoutCountingLiveDB(t *testing.T) {
	for _, tc := range []struct{ cap, prior int32 }{{3, 0}, {3, 2}, {3, 3}, {0, 0}} {
		t.Run(fmtCase(tc.cap, tc.prior), func(t *testing.T) {
			e := setupInterlockLiveDB(t)
			svc := e.capacityParkService(t, int(tc.cap))
			w := e.seedWorker(t, nil)
			id := e.fpSeedRunning(t, w, 1)
			hold := mhOpenHold(t, e, id, 1, w)
			e.exec(t, "UPDATE runs SET retained_park_count=$2, session_id='retained-session' WHERE id=$1", id, tc.prior)
			openBefore, totalBefore := e.countHolds(t, id)
			before, err := e.q.GetRunByID(e.ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			r, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, id, retainedCapacityReport(1))
			if err != nil || !applied {
				t.Fatalf("marked park applied=%v err=%v", applied, err)
			}
			if r.Status != "recovery_wait" || r.RecoveryWaitCause.String != recoveryCauseRetainedRecoveryExhausted || !r.RecoveryWaitCause.Valid ||
				r.RetainedParkCount != tc.prior || r.RecoveryRetryNotBefore.Valid ||
				!r.ClaimReleasedAt.Valid || r.WorkerID != before.WorkerID || r.SessionID != before.SessionID ||
				r.RecoveryWaitCount != before.RecoveryWaitCount+1 {
				t.Fatalf("marked park must hold uncounted with no retry: count=%d retry=%+v status=%s cause=%+v", r.RetainedParkCount, r.RecoveryRetryNotBefore, r.Status, r.RecoveryWaitCause)
			}
			if state, _ := e.fpHoldEvidence(t, hold); state != "open" {
				t.Fatal("park released custody")
			}
			if open, total := e.countHolds(t, id); open != openBefore || total != totalBefore {
				t.Fatalf("holds changed: open %d->%d total %d->%d", openBefore, open, totalBefore, total)
			}
			// Sweeper pass at a far-future clock: a NULL retry stamp is never promoted.
			if _, err := e.q.PromoteRecoveryWaitRuns(e.ctx, pgconv.Time(time.Now().Add(365*24*time.Hour))); err != nil {
				t.Fatal(err)
			}
			if n, err := e.q.PromoteRecoveryWaitRunNow(e.ctx, store.PromoteRecoveryWaitRunNowParams{ID: id, UserID: r.UserID}); err != nil || n != 0 {
				t.Fatalf("early promoter: %d %v", n, err)
			}
			if claimed, err := e.claimFor(t, w, r); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("claim poll must find nothing: id=%s err=%v", claimed.ID, err)
			}
			after, err := e.q.GetRunByID(e.ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if after.Status != "recovery_wait" || after.ClaimGeneration != 1 || after.RetainedParkCount != tc.prior || after.RecoveryRetryNotBefore.Valid {
				t.Fatalf("sweeper or claim moved the hold: %+v", after)
			}
			if open, total := e.countHolds(t, id); open != openBefore || total != totalBefore {
				t.Fatalf("sweeper or claim changed holds: open %d->%d total %d->%d", openBefore, open, totalBefore, total)
			}
		})
	}
}

func fmtCase(cap, prior int32) string { return fmt.Sprintf("cap%d_prior%d", cap, prior) }

// TestRetainedCapacityOnlyControlsLiveDB pins what the marker must NOT change: an unmarked park
// below the cap still counts with a timed retry, stale and released claims are refused with the
// row unchanged, and a duplicate of an applied marked park is fenced.
func TestRetainedCapacityOnlyControlsLiveDB(t *testing.T) {
	t.Run("unmarked below cap still counts", func(t *testing.T) {
		e := setupInterlockLiveDB(t)
		svc := e.capacityParkService(t, 3)
		w := e.seedWorker(t, nil)
		id := e.fpSeedRunning(t, w, 1)
		e.exec(t, "UPDATE runs SET retained_park_count=2 WHERE id=$1", id)
		r, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, id, retainedReport(1))
		if err != nil || !applied || r.RetainedParkCount != 3 || !r.RecoveryRetryNotBefore.Valid {
			t.Fatalf("unmarked park: applied=%v err=%v count=%d retry=%+v", applied, err, r.RetainedParkCount, r.RecoveryRetryNotBefore)
		}
	})
	for _, scenario := range []string{"stale", "released"} {
		t.Run(scenario+" marked park refused", func(t *testing.T) {
			e := setupInterlockLiveDB(t)
			svc := e.capacityParkService(t, 3)
			w := e.seedWorker(t, nil)
			id := e.fpSeedRunning(t, w, 2)
			hold := mhOpenHold(t, e, id, 2, w)
			gen := int64(2)
			if scenario == "stale" {
				gen = 1
			} else {
				e.exec(t, "UPDATE runs SET claim_released_at=now() WHERE id=$1", id)
			}
			before, err := e.q.GetRunByID(e.ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			_, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, id, retainedCapacityReport(gen))
			if applied || !errors.Is(err, ErrStaleClaim) {
				t.Fatalf("applied=%v err=%v, want ErrStaleClaim", applied, err)
			}
			after, err := e.q.GetRunByID(e.ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			a, _ := json.Marshal(before)
			b, _ := json.Marshal(after)
			if string(a) != string(b) {
				t.Fatal("refused marked park changed the run")
			}
			if state, _ := e.fpHoldEvidence(t, hold); state != "open" {
				t.Fatal("refused marked park released custody")
			}
		})
	}
	t.Run("duplicate after applied marked park is fenced", func(t *testing.T) {
		e := setupInterlockLiveDB(t)
		svc := e.capacityParkService(t, 3)
		w := e.seedWorker(t, nil)
		id := e.fpSeedRunning(t, w, 1)
		if _, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, id, retainedCapacityReport(1)); err != nil || !applied {
			t.Fatalf("first: %v %v", applied, err)
		}
		if _, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, id, retainedCapacityReport(1)); applied || !errors.Is(err, ErrStaleClaim) {
			t.Fatalf("duplicate: %v %v", applied, err)
		}
		r, err := e.q.GetRunByID(e.ctx, id)
		if err != nil || r.RecoveryWaitCount != 1 || r.RetainedParkCount != 0 {
			t.Fatalf("duplicate changed counters: wait=%d retained=%d err=%v", r.RecoveryWaitCount, r.RetainedParkCount, err)
		}
	})
}

// TestRetainedCapacityOnlyOwnerResumeThenHoldsAgainLiveDB: the owner Resume of the held park
// requeues the run and renews the stored episode; a fresh claim and a second marked park hold
// again with the retained counter still unchanged.
func TestRetainedCapacityOnlyOwnerResumeThenHoldsAgainLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.capacityParkService(t, 3)
	w := e.seedWorker(t, nil)
	id := e.fpSeedRunning(t, w, 1)
	hold := mhOpenHold(t, e, id, 1, w)
	e.exec(t, "UPDATE runs SET retained_park_count=2, worker_recovery_episode=2, started_at=now()-interval '10 minutes', budget_wall_seconds=3600 WHERE id=$1", id)
	r, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, id, retainedCapacityReport(1))
	if err != nil || !applied || r.RetainedParkCount != 2 || r.RecoveryRetryNotBefore.Valid {
		t.Fatalf("first marked park: applied=%v err=%v count=%d retry=%+v", applied, err, r.RetainedParkCount, r.RecoveryRetryNotBefore)
	}
	if _, err := e.q.ResumeWorkerRecoveryEpisode(e.ctx, store.ResumeWorkerRecoveryEpisodeParams{ID: id, UserID: r.UserID, GlobalTimeoutSeconds: 3600}); err != nil {
		t.Fatalf("owner resume: %v", err)
	}
	resumed, err := e.q.GetRunByID(e.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Status != "queued" || resumed.WorkerRecoveryEpisode != 3 || resumed.RetainedParkCount != 2 || resumed.RecoveryWaitCause.Valid {
		t.Fatalf("resume: status=%s episode=%d retained=%d cause=%+v", resumed.Status, resumed.WorkerRecoveryEpisode, resumed.RetainedParkCount, resumed.RecoveryWaitCause)
	}
	claimed, err := e.claimFor(t, w, resumed)
	if err != nil || claimed.ID != id || claimed.ClaimGeneration != 2 || claimed.RetainedParkCount != 2 {
		t.Fatalf("fresh claim: gen=%d err=%v", claimed.ClaimGeneration, err)
	}
	gen := claimed.ClaimGeneration
	if _, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, id, StateRequest{State: "running", ClaimGeneration: &gen}); err != nil || !applied {
		t.Fatalf("fresh execution: %v %v", applied, err)
	}
	r2, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, id, retainedCapacityReport(gen))
	if err != nil || !applied || r2.Status != "recovery_wait" || r2.RetainedParkCount != 2 || r2.RecoveryRetryNotBefore.Valid ||
		r2.RecoveryWaitCause.String != recoveryCauseRetainedRecoveryExhausted {
		t.Fatalf("second marked park: applied=%v err=%v status=%s count=%d retry=%+v", applied, err, r2.Status, r2.RetainedParkCount, r2.RecoveryRetryNotBefore)
	}
	if state, _ := e.fpHoldEvidence(t, hold); state != "open" {
		t.Fatal("custody released across resume and re-park")
	}
}
