package workersvc

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestDiskParkHoldPreservesStateAndFencesLiveDB(t *testing.T) {
	for _, guarded := range []bool{false, true} {
		t.Run(fmt.Sprint(guarded), func(t *testing.T) {
			e := setupInterlockLiveDB(t)
			svc := e.diskParkService(t, 1)
			w := e.seedWorker(t, nil)
			id := e.fpSeedRunning(t, w, 2)
			e.exec(t, `UPDATE runs SET started_at=now(),disk_park_count=1,recovery_wait_count=7,session_id='retained',
    budget_extension_seconds=123,budget_finalize_seconds=321,worker_recovery_episode=3,
    requeue_count=9,requeue_episode_baseline=5,memory_episode=2,provider_park_count=4,
    provider_park_streak_started_at=now(),iteration_count=5 WHERE id=$1`, id)
			// Include an older generation: disk holds protect every retained source.
			for _, gen := range []int64{1, 2} {
				e.exec(t, `INSERT INTO recovery_custody_holds
     (user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id,inventory_guarded)
     VALUES ($1,$2,$3,$4,'open',$5,'fixture',$5,$3,$6)`, e.userID, e.repoID, id, gen, w, guarded)
			}
			before := e.fpPreserved(t, id)
			run, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, id, dataVolumeFullReport(2, nil))
			if err != nil || !applied || !isDataVolumeFullHeld(run) || run.RecoveryWaitCount != 8 || e.fpPreserved(t, id) != before {
				t.Fatalf("hold status=%s count=%d applied=%v err=%v", run.Status, run.RecoveryWaitCount, applied, err)
			}
			readout, err := store.ReadMaintenanceReadout(e.ctx, e.pool)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, item := range readout.Held.Items {
				if item.RunID == id {
					found = true
					if item.Tag == nil || *item.Tag != "needs_owner" {
						t.Fatalf("disk hold maintenance tag=%v", item.Tag)
					}
				}
			}
			if !found {
				t.Fatal("disk hold absent from maintenance held section")
			}
			var sourceBefore string
			sourceSnapshot := func() string {
				var value string
				if err := e.pool.QueryRow(e.ctx, `SELECT jsonb_agg(to_jsonb(h) ORDER BY id)::text FROM recovery_custody_holds h WHERE run_id=$1`, id).Scan(&value); err != nil {
					t.Fatal(err)
				}
				return value
			}
			sourceBefore = sourceSnapshot()
			// A duplicate and late noncancel reports cannot consume counts, grants or source.
			for _, state := range []string{"running", "failed", "completed", "awaiting_input", "awaiting_followup", "awaiting_approval", "recovery_wait"} {
				req := StateRequest{State: state, ClaimGeneration: i64Ptr(2)}
				if state == "recovery_wait" {
					req = dataVolumeFullReport(2, nil)
				}
				if _, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, id, req); err != nil || applied {
					t.Fatalf("late %s=%v/%v", state, applied, err)
				}
			}
			if _, _, err := svc.SetState(e.ctx, store.Worker{ID: w}, id, dataVolumeFullReport(1, nil)); !errors.Is(err, ErrStaleClaim) {
				t.Fatalf("stale=%v", err)
			}
			if n, err := e.q.PromoteRecoveryWaitRunNow(e.ctx, store.PromoteRecoveryWaitRunNowParams{ID: id, UserID: e.userID}); err != nil || n != 0 {
				t.Fatalf("early=%d/%v", n, err)
			}
			if _, err := e.q.PromoteRecoveryWaitRuns(e.ctx, pgconv.Time(time.Now().Add(365*24*time.Hour))); err != nil {
				t.Fatal(err)
			}
			if _, err := e.q.PromoteVaultLockedRecoveryWaitRuns(e.ctx, e.userID); err != nil {
				t.Fatal(err)
			}
			for _, gen := range []int64{1, 2} {
				if n, err := e.q.ReleaseCustodyHoldExact(e.ctx, store.ReleaseCustodyHoldExactParams{RunID: id, Generation: gen, WorkerID: w, ReleaseEvidence: pgconv.TextOrNull("forge_no_output")}); err != nil || n != 0 {
					t.Fatalf("release gen%d=%d/%v", gen, n, err)
				}
			}
			current, err := e.q.GetRunOwnedByWorker(e.ctx, store.GetRunOwnedByWorkerParams{ID: id, WorkerID: pgconv.UUID(w)})
			if err != nil || !isDataVolumeFullHeld(current) || current.RecoveryWaitCount != 8 || sourceSnapshot() != sourceBefore || e.fpPreserved(t, id) != before {
				t.Fatal("late report or promoter changed hold")
			}
			// Resume banks time but grants no fresh allowance and fences the old flight.
			e.exec(t, `UPDATE runs SET status_since=now()-interval '60 seconds' WHERE id=$1`, id)
			resumed, err := e.q.ResumeDataVolumeFullHold(e.ctx, store.ResumeDataVolumeFullHoldParams{ID: id, UserID: e.userID, GlobalTimeoutSeconds: 86400})
			if err != nil || resumed.Status != "queued" || !resumed.ClaimReleasedAt.Valid || resumed.BudgetPausedSeconds < 60 ||
				resumed.StartedAt != run.StartedAt || resumed.WorkerID != run.WorkerID || resumed.SessionID != run.SessionID ||
				resumed.DiskParkCount != 1 || resumed.RequeueCount != run.RequeueCount || resumed.WorkerRecoveryEpisode != run.WorkerRecoveryEpisode ||
				resumed.BudgetExtensionSeconds != 123 || resumed.BudgetFinalizeSeconds != 321 || sourceSnapshot() != sourceBefore {
				t.Fatalf("resume=%+v err=%v", resumed, err)
			}
			if _, err := e.q.ResumeDataVolumeFullHold(e.ctx, store.ResumeDataVolumeFullHoldParams{ID: id, UserID: e.userID, GlobalTimeoutSeconds: 86400}); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("duplicate resume=%v", err)
			}
			queuedBefore, err := e.q.GetRunByID(e.ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			for _, state := range []string{"failed", "completed", "awaiting_input", "awaiting_followup", "recovery_wait"} {
				req := StateRequest{State: state, ClaimGeneration: i64Ptr(2)}
				if state == "recovery_wait" {
					req = dataVolumeFullReport(2, nil)
				}
				if _, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, id, req); !errors.Is(err, ErrStaleClaim) || applied {
					t.Fatalf("queued old flight %s applied=%v err=%v", state, applied, err)
				}
			}
			queuedAfter, err := e.q.GetRunByID(e.ctx, id)
			if err != nil || !reflect.DeepEqual(queuedBefore, queuedAfter) || sourceSnapshot() != sourceBefore {
				t.Fatal("queued old reports mutated run or custody")
			}
		})
	}
}

func TestDiskHoldResumeGuardsLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	w := e.seedWorker(t, nil)
	for i, mutation := range []string{
		"recovery_retry_not_before=now()+interval '1 hour'",
		"stop_kind='cancelled'",
		"status='failed',failure_reason='existing failure'",
		"started_at=now()-interval '2 hours',status_since=now(),budget_wall_seconds=1",
	} {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			id := e.fpSeedRunning(t, w, 1)
			e.exec(t, `UPDATE runs SET status='recovery_wait',recovery_wait_cause='data_volume_full',recovery_retry_not_before=NULL WHERE id=$1`, id)
			e.exec(t, "UPDATE runs SET "+mutation+" WHERE id=$1", id)
			before, err := e.q.GetRunByID(e.ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := e.q.ResumeDataVolumeFullHold(e.ctx, store.ResumeDataVolumeFullHoldParams{ID: id, UserID: e.userID, GlobalTimeoutSeconds: 3600}); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("ineligible resume=%v", err)
			}
			after, err := e.q.GetRunByID(e.ctx, id)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("refused resume mutated run")
			}
		})
	}
	id := e.fpSeedRunning(t, w, 1)
	e.exec(t, `UPDATE runs SET status='recovery_wait',recovery_wait_cause='data_volume_full',recovery_retry_not_before=NULL WHERE id=$1`, id)
	if _, err := e.q.ResumeDataVolumeFullHold(e.ctx, store.ResumeDataVolumeFullHoldParams{ID: id, UserID: uuid.New(), GlobalTimeoutSeconds: 3600}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("foreign resume=%v", err)
	}
}

func TestDiskHoldConcurrentReportAndResumeLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.diskParkService(t, 1)
	w := e.seedWorker(t, nil)
	id := e.fpSeedRunning(t, w, 2)
	e.exec(t, `UPDATE runs SET disk_park_count=1,recovery_wait_count=7 WHERE id=$1`, id)
	start := make(chan struct{})
	type result struct {
		applied bool
		err     error
	}
	results := make(chan result, 2)
	for range 2 {
		go func() {
			<-start
			_, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, id, dataVolumeFullReport(2, nil))
			results <- result{applied, err}
		}()
	}
	close(start)
	a, b := <-results, <-results
	if a.err != nil || b.err != nil || a.applied == b.applied {
		t.Fatalf("concurrent disk reports: %+v / %+v", a, b)
	}
	held, err := e.q.GetRunByID(e.ctx, id)
	if err != nil || !isDataVolumeFullHeld(held) || held.DiskParkCount != 1 || held.RecoveryWaitCount != 8 {
		t.Fatalf("concurrent count=%+v err=%v", held, err)
	}
	before := held
	wrong := e.seedWorker(t, nil)
	if _, applied, err := svc.SetState(e.ctx, store.Worker{ID: wrong}, id, dataVolumeFullReport(2, nil)); !errors.Is(err, ErrRunNotOwned) || applied {
		t.Fatalf("wrong worker applied=%v err=%v", applied, err)
	}
	if _, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, id, dataVolumeFullReport(1, nil)); !errors.Is(err, ErrStaleClaim) || applied {
		t.Fatalf("stale report applied=%v err=%v", applied, err)
	}
	after, err := e.q.GetRunByID(e.ctx, id)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("wrong worker/stale report mutated held row")
	}
	resume := make(chan error, 2)
	start = make(chan struct{})
	for range 2 {
		go func() {
			<-start
			_, err := e.q.ResumeDataVolumeFullHold(e.ctx, store.ResumeDataVolumeFullHoldParams{ID: id, UserID: e.userID, GlobalTimeoutSeconds: 86400})
			resume <- err
		}()
	}
	close(start)
	x, y := <-resume, <-resume
	if !((x == nil && errors.Is(y, pgx.ErrNoRows)) || (y == nil && errors.Is(x, pgx.ErrNoRows))) {
		t.Fatalf("resume race=%v/%v", x, y)
	}
}

func TestDiskHoldResumeWallCreditLiveDB(t *testing.T) {
	for _, started := range []bool{false, true} {
		t.Run(fmt.Sprint(started), func(t *testing.T) {
			e := setupInterlockLiveDB(t)
			w := e.seedWorker(t, nil)
			id := e.fpSeedRunning(t, w, 2)
			e.exec(t, `UPDATE runs SET status='recovery_wait',recovery_wait_cause='data_volume_full',recovery_retry_not_before=NULL,
 started_at=CASE WHEN $2::boolean THEN now()-interval '2 hours' END,status_since=now()-interval '1 hour',
 budget_paused_seconds=37,budget_wall_seconds=86400,budget_extension_seconds=123,budget_finalize_seconds=321,
 disk_park_count=1,requeue_count=9,requeue_episode_baseline=5,worker_recovery_episode=3,memory_episode=2 WHERE id=$1`, id, started)
			before, err := e.q.GetRunByID(e.ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			got, err := e.q.ResumeDataVolumeFullHold(e.ctx, store.ResumeDataVolumeFullHoldParams{ID: id, UserID: e.userID, GlobalTimeoutSeconds: 86400})
			if err != nil {
				t.Fatal(err)
			}
			want := int32(37)
			if started {
				want += int32(got.StatusSince.Time.Sub(before.StatusSince.Time).Seconds() + 0.5)
			}
			if got.BudgetPausedSeconds != want {
				t.Fatalf("paused credit=%d want %d", got.BudgetPausedSeconds, want)
			}
			// Normalize only the documented transition fields, then compare every other field.
			normalized := got
			normalized.Status = before.Status
			normalized.StatusSince = before.StatusSince
			normalized.BudgetPausedSeconds = before.BudgetPausedSeconds
			normalized.RecoveryWaitCause = before.RecoveryWaitCause
			normalized.RecoveryRetryNotBefore = before.RecoveryRetryNotBefore
			normalized.ClaimReleasedAt = before.ClaimReleasedAt
			normalized.CodexCapHash = before.CodexCapHash
			normalized.CodexClaimEpoch = before.CodexClaimEpoch
			normalized.Health = before.Health
			normalized.HealthReason = before.HealthReason
			normalized.HealthSince = before.HealthSince
			normalized.UpdatedAt = before.UpdatedAt
			if got.Status != "queued" || !got.ClaimReleasedAt.Valid || !reflect.DeepEqual(before, normalized) {
				t.Fatal("resume reset unrelated run history or budget grants")
			}
		})
	}
}
