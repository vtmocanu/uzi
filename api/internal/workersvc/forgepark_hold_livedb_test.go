package workersvc

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Compare all stored fields except the documented transition fields. This includes
// grants, budgets, recovery episodes, claim posture and frozen source identity.
func (e interlockLiveDB) fpPreserved(t *testing.T, id uuid.UUID) string {
	t.Helper()
	var v string
	if err := e.pool.QueryRow(e.ctx, `SELECT (to_jsonb(r) - ARRAY[
 'status','status_since','recovery_wait_cause','recovery_retry_not_before',
 'recovery_wait_count','session_id','health','health_reason','health_since','updated_at'
 ])::text FROM runs r WHERE id=$1`, id).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// The last allowed park still has a finite deadline; only the NEXT group holds.
func TestForgeParkLastTimedAllowanceLiveDB(t *testing.T) {
	for _, cap := range []int{1, 6} {
		t.Run(fmt.Sprint(cap), func(t *testing.T) {
			e := setupInterlockLiveDB(t)
			svc := e.forgeParkService(t, cap)
			w := e.seedWorker(t, nil)
			id := e.fpSeedRunning(t, w, 1)
			e.exec(t, `UPDATE runs SET forge_park_count=$2 WHERE id=$1`, id, cap-1)
			mhOpenHold(t, e, id, 1, w)
			run, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, id, fpForgeParkReport())
			if err != nil || !applied || !run.RecoveryRetryNotBefore.Valid || int(run.ForgeParkCount) != cap {
				t.Fatalf("last timed allowance count=%d retry=%v applied=%v err=%v", run.ForgeParkCount, run.RecoveryRetryNotBefore.Valid, applied, err)
			}
			// Generic early promotion remains available for a finite forge park.
			if n, err := e.q.PromoteRecoveryWaitRunNow(e.ctx, store.PromoteRecoveryWaitRunNowParams{ID: id, UserID: e.userID}); err != nil || n != 1 {
				t.Fatalf("finite early promotion=%d/%v", n, err)
			}
		})
	}
}

func TestForgeParkFiniteAllowanceAndReholdLiveDB(t *testing.T) {
	for _, cap := range []int{1, 6, 0} {
		for _, guarded := range []bool{false, true} {
			t.Run(fmt.Sprintf("cap%d/guarded%v", cap, guarded), func(t *testing.T) {
				e := setupInterlockLiveDB(t)
				svc := e.forgeParkService(t, cap)
				w := e.seedWorker(t, nil)
				id := e.fpSeedRunning(t, w, 1)
				prior := cap
				if cap == 0 {
					prior = 99
				}
				e.exec(t, `UPDATE runs SET forge_park_count=$2,session_id='retained-session',recovery_wait_count=7,
      budget_extension_seconds=123,budget_finalize_seconds=321,worker_recovery_episode=3,requeue_count=9,requeue_episode_baseline=5,memory_episode=2,reserve_exempt_granted_at=now(),reserve_exempt_until=now()+interval '1 day',provider_park_count=4,
      provider_park_streak_started_at=now(),iteration_count=5 WHERE id=$1`, id, prior)
				hold := uuid.New()
				e.exec(t, `INSERT INTO recovery_custody_holds
 (id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id,inventory_guarded)
 VALUES ($1,$2,$3,$4,1,'open',$5,'fixture',$5,$4,$6)`, hold, e.userID, e.repoID, id, w, guarded)
				stored, err := e.q.GetCustodyHoldForSettle(e.ctx, store.GetCustodyHoldForSettleParams{HoldID: hold, RunID: id})
				if err != nil || stored.InventoryGuarded != guarded {
					t.Fatalf("immutable fixture guard=%v/%v", stored.InventoryGuarded, err)
				}
				if guarded {
					e.exec(t, `INSERT INTO recovery_captures
      (id,hold_id,run_id,user_id,original_worker_identity,source_sha,idempotency_key,state)
      VALUES ($1,$2,$3,$4,'fixture','abc',$5,'preparing')`, uuid.New(), hold, id, e.userID, uuid.NewString())
				}
				sourceSnapshot := func() string {
					var v string
					if err := e.pool.QueryRow(e.ctx, `SELECT jsonb_build_object(
      'hold',(SELECT to_jsonb(h) FROM recovery_custody_holds h WHERE h.id=$1),
      'captures',(SELECT COALESCE(jsonb_agg(to_jsonb(c) ORDER BY c.id),'[]'::jsonb) FROM recovery_captures c WHERE c.hold_id=$1))::text`, hold).Scan(&v); err != nil {
						t.Fatal(err)
					}
					return v
				}
				sourceBefore := sourceSnapshot()
				before := e.fpPreserved(t, id)
				run, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, id, fpForgeParkReport())
				if err != nil || !applied || run.Status != "recovery_wait" || run.RecoveryWaitCause.String != "forge_unreachable" ||
					run.SessionID.String != "retained-session" || run.RecoveryWaitCount != 8 {
					t.Fatalf("park status=%s cause=%v session=%v backoff=%d applied=%v err=%v", run.Status, run.RecoveryWaitCause, run.SessionID, run.RecoveryWaitCount, applied, err)
				}
				first := e.originCooldown(t, w, "https://forge.e2e")
				if first.ObservationVersion != 1 {
					t.Fatalf("observation=%d", first.ObservationVersion)
				}
				if cap == 0 {
					if !run.RecoveryRetryNotBefore.Valid || run.ForgeParkCount != 100 {
						t.Fatal("unlimited park did not consume timed allowance")
					}
					return
				}
				if run.RecoveryRetryNotBefore.Valid || int(run.ForgeParkCount) != cap || e.fpPreserved(t, id) != before {
					t.Fatal("cap hold changed preserved state or consumed allowance")
				}
				if state, ev := e.fpHoldEvidence(t, hold); state != "open" || ev != nil {
					t.Fatal("cap hold released custody")
				}
				if sourceSnapshot() != sourceBefore {
					t.Fatal("cap hold changed source inventory")
				}
				// Lost acknowledgements cannot manufacture a new observation or backoff step.
				_, applied, err = svc.SetState(e.ctx, store.Worker{ID: w}, id, fpForgeParkReport())
				if err != nil || applied {
					t.Fatalf("duplicate=%v/%v", applied, err)
				}
				if next := e.originCooldown(t, w, "https://forge.e2e"); next.ObservationVersion != 1 || !next.CooledUntil.Time.Equal(first.CooledUntil.Time) {
					t.Fatal("duplicate changed cooldown")
				}
				if _, err := e.q.PromoteRecoveryWaitRuns(e.ctx, pgconv.Time(time.Now().Add(365*24*time.Hour))); err != nil {
					t.Fatal(err)
				}
				if n, err := e.q.PromoteRecoveryWaitRunNow(e.ctx, store.PromoteRecoveryWaitRunNowParams{ID: id, UserID: e.userID}); err != nil || n != 0 {
					t.Fatalf("generic promote=%d/%v", n, err)
				}
				if _, err := e.q.PromoteVaultLockedRecoveryWaitRuns(e.ctx, e.userID); err != nil {
					t.Fatal(err)
				}
				if _, err := e.q.ResumeForgeUnreachableHold(e.ctx, store.ResumeForgeUnreachableHoldParams{ID: id, UserID: uuid.New(), GlobalTimeoutSeconds: 86400}); !errors.Is(err, pgx.ErrNoRows) {
					t.Fatalf("foreign resume=%v", err)
				}
				if after := e.fpPreserved(t, id); after != before {
					t.Fatal("refused promoter changed preserved state")
				}
				resumed, err := e.q.ResumeForgeUnreachableHold(e.ctx, store.ResumeForgeUnreachableHoldParams{ID: id, UserID: e.userID, GlobalTimeoutSeconds: 86400})
				if err != nil || resumed.Status != "queued" || resumed.ForgeParkCount != run.ForgeParkCount || resumed.WorkerID != run.WorkerID || resumed.SessionID != run.SessionID || resumed.ClaimGeneration != run.ClaimGeneration {
					t.Fatalf("resume=%s/%v", resumed.Status, err)
				}
				if next := e.originCooldown(t, w, "https://forge.e2e"); next.ObservationVersion != 1 || !next.CooledUntil.Time.Equal(first.CooledUntil.Time) {
					t.Fatal("Resume treated cooldown as healthy")
				}
				if sourceSnapshot() != sourceBefore {
					t.Fatal("Resume changed source inventory")
				}
				// Model the next ordinary flight at the service's report seam, retaining its
				// source hold. A further exhausted group reholds without new timed allowance.
				e.exec(t, `UPDATE runs SET status='running' WHERE id=$1`, id)
				run, applied, err = svc.SetState(e.ctx, store.Worker{ID: w}, id, fpForgeParkReport())
				if err != nil || !applied || run.Status != "recovery_wait" || run.RecoveryRetryNotBefore.Valid || int(run.ForgeParkCount) != cap {
					t.Fatalf("rehold=%s/%v/%v", run.Status, applied, err)
				}
				if next := e.originCooldown(t, w, "https://forge.e2e"); next.ObservationVersion != 2 || next.FailureStep != 1 {
					t.Fatal("rehold did not observe exactly one failed group")
				}
			})
		}
	}
}

// Exercise the actual service claim blocker, rather than only counting custody.
func TestForgeParkCapHoldCustodySaturationLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	f := newLiveCustodyClaimFixture(t, env, 8, true)
	f.svc.p.RunForgeUnreachableMaxParks = 1
	for i, id := range f.runs {
		env.exec(`UPDATE runs SET forge_park_count=1 WHERE id=$1`, id)
		_, applied, err := f.svc.SetState(env.ctx, store.Worker{ID: f.workers[i]}, id, fpForgeParkReport())
		if err != nil || !applied {
			t.Fatalf("hold[%d]=%v/%v", i, applied, err)
		}
	}
	fresh := f.seedRun("queued", 0)
	if got, _ := f.claim(t); got != uuid.Nil {
		t.Fatalf("saturated owner claimed %s (fresh=%s)", got, fresh)
	}
	if f.openHolds(t) != 8 {
		t.Fatal("cap holds escaped custody saturation")
	}
	// Positive control: freeing one fixture slot permits that same ordinary claim.
	env.exec(`DELETE FROM recovery_custody_holds WHERE id=$1`, f.holds[0])
	if got, _ := f.claim(t); got != fresh {
		t.Fatalf("claim after freeing custody=%s want %s", got, fresh)
	}
}

// Owner Resume does not grant a custody exemption, even after origin cooldown is due.
func TestForgeParkResumeCustodyLimitOneLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	f := newCustodyContinuationFixture(t, env)
	f.svc.p.CustodyHoldLimit = &fakeCustodyLimit{n: 1}
	f.svc.p.RunForgeUnreachableMaxParks = 1
	id := f.seedRun("running", 1)
	env.exec(`UPDATE runs SET worker_id=$2,forge_park_count=1 WHERE id=$1`, id, f.workerID)
	env.exec(`INSERT INTO recovery_custody_holds
 (user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id,inventory_guarded)
 VALUES ($1,$2,$3,1,'open',$4,'fixture',$4,$3,true)`, f.userID, f.repoID, id, f.workerID)
	if n := f.openHolds(t); n != 1 {
		t.Fatalf("fixture holds=%d", n)
	}
	held, applied, err := f.svc.SetState(env.ctx, f.wkr, id, fpForgeParkReport())
	if err != nil || !applied || held.Status != "recovery_wait" || held.RecoveryRetryNotBefore.Valid {
		t.Fatalf("hold=%s/%v/%v", held.Status, applied, err)
	}
	if _, err := env.q.ResumeForgeUnreachableHold(env.ctx, store.ResumeForgeUnreachableHoldParams{ID: id, UserID: f.userID, GlobalTimeoutSeconds: 3600}); err != nil {
		t.Fatal(err)
	}
	env.exec(`UPDATE forge_origin_cooldowns SET cooled_until=now()-interval '1 second' WHERE user_id=$1`, f.userID)
	if got, _ := f.claim(t); got != uuid.Nil {
		t.Fatalf("Resume bypassed own custody limit: %s", got)
	}
	row := store.ListActiveRunsForHealthRow{ID: id, UserID: f.userID, Status: "queued", Kind: "issue"}
	if got := f.svc.queuedReason(env.ctx, time.Now(), row, thWithLimit(1)); got != reasonCustodyLimit {
		t.Fatalf("reason=%q want %q", got, reasonCustodyLimit)
	}
	run, err := env.q.GetRunByID(env.ctx, id)
	if err != nil || run.ClaimGeneration != 1 || run.ForgeParkCount != 1 || f.openHolds(t) != 1 {
		t.Fatalf("blocked Resume changed counts/generation: %d/%d/%v", run.ClaimGeneration, run.ForgeParkCount, err)
	}
}

func TestForgeParkResumeOneOrdinaryAttemptLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	f := newCustodyContinuationFixture(t, env)
	f.svc.p.RunForgeUnreachableMaxParks = 1
	id := f.seedRun("running", 1)
	env.exec(`UPDATE runs SET worker_id=$2,forge_park_count=1 WHERE id=$1`, id, f.workerID)
	env.exec(`INSERT INTO recovery_custody_holds
 (user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id,inventory_guarded)
 VALUES ($1,$2,$3,1,'open',$4,'fixture',$4,$3,true)`, f.userID, f.repoID, id, f.workerID)
	if _, applied, err := f.svc.SetState(env.ctx, f.wkr, id, fpForgeParkReport()); err != nil || !applied {
		t.Fatalf("initial hold=%v/%v", applied, err)
	}
	if _, err := env.q.ResumeForgeUnreachableHold(env.ctx, store.ResumeForgeUnreachableHoldParams{ID: id, UserID: f.userID, GlobalTimeoutSeconds: 3600}); err != nil {
		t.Fatal(err)
	}
	env.exec(`UPDATE forge_origin_cooldowns SET cooled_until=now()-interval '1 second' WHERE user_id=$1`, f.userID)
	got, g := f.claim(t)
	if got != id || g != 2 {
		t.Fatalf("ordinary Resume claim=%s/%d want %s/2", got, g, id)
	}
	if _, applied, err := f.svc.SetState(env.ctx, f.wkr, id, StateRequest{State: "running", ClaimGeneration: i64Ptr(g)}); err != nil || !applied {
		t.Fatalf("running=%v/%v", applied, err)
	}
	held, applied, err := f.svc.SetState(env.ctx, f.wkr, id, StateRequest{State: "recovery_wait", RecoveryCause: strPtr("forge_unreachable"), ClaimGeneration: i64Ptr(g)})
	if err != nil || !applied || held.Status != "recovery_wait" || held.RecoveryRetryNotBefore.Valid || held.ForgeParkCount != 1 {
		t.Fatalf("rehold=%s/%v/%v", held.Status, applied, err)
	}
	if f.ownOpenHolds(t, id) != 2 {
		t.Fatal("ordinary retry released a retained generation")
	}
	var version int64
	if err := env.pool.QueryRow(env.ctx, `SELECT observation_version FROM forge_origin_cooldowns WHERE user_id=$1 AND worker_id=$2`, f.userID, f.workerID).Scan(&version); err != nil || version != 2 {
		t.Fatalf("observations=%d/%v", version, err)
	}
}
