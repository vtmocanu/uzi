package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func retainedReport(gen int64) StateRequest {
	return StateRequest{State: "recovery_wait", RecoveryCause: strPtr(recoveryCauseRetainedRecoveryExhausted), ClaimGeneration: &gen}
}

func TestRetainedParkCountsAndResumeLiveDB(t *testing.T) {
	for _, cap := range []int{0, 3, 5} {
		t.Run(fmt.Sprint(cap), func(t *testing.T) {
			e := setupInterlockLiveDB(t)
			p := testParams()
			p.RunRetainedRecoveryMaxParks = cap
			svc := New(e.q, newBox(t), p)
			svc.SetTxBeginner(e.pool)
			svc.SetBackground(func(func()) {})
			w := e.seedWorker(t, nil)
			id := e.fpSeedRunning(t, w, 1)
			hold := mhOpenHold(t, e, id, 1, w)
			e.exec(t, "UPDATE runs SET recovery_wait_count=99, session_id='retained-session', worker_recovery_episode=2, requeue_count=7, requeue_episode_baseline=4, started_at=now()-interval '10 minutes', budget_wall_seconds=3600, budget_paused_seconds=17 WHERE id=$1", id)
			gen := int64(1)
			for prior := 0; prior <= cap; prior++ {
				before, err := e.q.GetRunByID(e.ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				start := time.Now()
				r, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, id, retainedReport(gen))
				if err != nil || !applied {
					t.Fatalf("park prior=%d: applied=%v err=%v", prior, applied, err)
				}
				want := int32(prior)
				if prior < cap {
					want++
				}
				if r.Status != "recovery_wait" || r.RecoveryWaitCause.String != recoveryCauseRetainedRecoveryExhausted ||
					r.RetainedParkCount != want || r.RecoveryWaitCount != before.RecoveryWaitCount+1 ||
					!r.ClaimReleasedAt.Valid || r.WorkerID != before.WorkerID || r.SessionID != before.SessionID ||
					r.WorkerRecoveryEpisode != 2 || r.RequeueCount != 7 || r.RequeueEpisodeBaseline != before.RequeueEpisodeBaseline ||
					r.ReleasedWorkerID != before.ReleasedWorkerID || r.ReleasedWorkerNonce != before.ReleasedWorkerNonce || r.FinishedAt.Valid {
					t.Fatalf("park counters, affinity or fences wrong: %+v", r)
				}
				if state, _ := e.fpHoldEvidence(t, hold); state != "open" {
					t.Fatal("park released custody")
				}
				if prior < cap {
					delay := r.RecoveryRetryNotBefore.Time.Sub(start)
					base := time.Minute * time.Duration(1<<uint(prior))
					if !r.RecoveryRetryNotBefore.Valid || delay < base+recoveryParkJitterMin-time.Second || delay > base+recoveryParkJitterMax+time.Second {
						t.Fatalf("delay=%s expected base=%s plus jitter", delay, base)
					}
				} else if r.RecoveryRetryNotBefore.Valid {
					t.Fatal("cap must hold without timer")
				}
				if _, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, id, retainedReport(gen)); applied || !errors.Is(err, ErrStaleClaim) {
					t.Fatalf("duplicate: %v %v", applied, err)
				}
				n, err := e.q.PromoteRecoveryWaitRunNow(e.ctx, store.PromoteRecoveryWaitRunNowParams{ID: id, UserID: r.UserID})
				if err != nil || n != 0 {
					t.Fatalf("early promoter: %d %v", n, err)
				}
				if prior < cap {
					e.exec(t, "UPDATE runs SET recovery_retry_not_before=now()-interval '1 second' WHERE id=$1", id)
				}
				_, err = e.q.PromoteRecoveryWaitRuns(e.ctx, pgconv.Time(time.Now()))
				if err != nil {
					t.Fatal(err)
				}
				after, err := e.q.GetRunByID(e.ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				if prior == cap {
					if after.Status != "recovery_wait" {
						t.Fatal("timer promoted owner hold")
					}
					_, err = e.q.ResumeWorkerRecoveryEpisode(e.ctx, store.ResumeWorkerRecoveryEpisodeParams{ID: id, UserID: r.UserID, GlobalTimeoutSeconds: 3600})
					if err != nil {
						t.Fatal(err)
					}
					after, err = e.q.GetRunByID(e.ctx, id)
					if err != nil {
						t.Fatal(err)
					}
					if after.WorkerRecoveryEpisode != 3 || after.RequeueEpisodeBaseline != 7 {
						t.Fatal("owner resume did not renew stored episode")
					}
				}
				if after.Status != "queued" || !after.ClaimReleasedAt.Valid || after.WorkerID != r.WorkerID || after.RetainedParkCount != want ||
					(prior == cap && after.RecoveryWaitCause.Valid) ||
					(prior < cap && after.RecoveryWaitCause.String != recoveryCauseRetainedRecoveryExhausted) {
					t.Fatalf("promotion/resume lost fence or affinity: %+v", after)
				}
				if prior < cap && (after.StartedAt.Valid || after.BudgetPausedSeconds != 0) {
					t.Fatal("automatic promotion must reset the wall baseline")
				}
				if prior == cap && after.StartedAt != r.StartedAt {
					t.Fatal("owner Resume must preserve the wall baseline")
				}
				// Real same-worker claim must advance generation and clear only the execution fence.
				claimed, err := e.q.ClaimRun(e.ctx, store.ClaimRunParams{WorkerID: pgconv.UUID(w), UserID: r.UserID,
					IsDockerWorker: true, DockerRepoAllowlist: []uuid.UUID{r.RepoID.Bytes},
					AffinityCutoff: pgconv.Time(time.Now().Add(-time.Hour)), HeartbeatCutoff: pgconv.Time(time.Now().Add(-time.Hour)),
					RecoveryCapable: true, WorkerIdentity: "retained-worker", WorkerProtocolCaps: []string{"recovery_inventory_v1"}})
				if err != nil || claimed.ID != id || claimed.ClaimGeneration != gen+1 || claimed.ClaimReleasedAt.Valid || claimed.RetainedParkCount != want {
					t.Fatalf("fresh claim: gen=%d err=%v", claimed.ClaimGeneration, err)
				}
				gen = claimed.ClaimGeneration
				_, applied, err = svc.SetState(e.ctx, store.Worker{ID: w}, id, StateRequest{State: "running", ClaimGeneration: &gen})
				if err != nil || !applied {
					t.Fatalf("fresh execution: %v %v", applied, err)
				}
			}
		})
	}
}

func TestRetainedParkRefusalsWriteNothingLiveDB(t *testing.T) {
	for _, scenario := range []string{"stale", "released", "stop", "cancelled", "newer outcome", "chat", "job", "judge", "cross_check"} {
		t.Run(scenario, func(t *testing.T) {
			e := setupInterlockLiveDB(t)
			svc := e.diskParkService(t, 3)
			w := e.seedWorker(t, nil)
			id := e.fpSeedRunning(t, w, 2)
			hold := mhOpenHold(t, e, id, 2, w)
			gen := int64(2)
			switch scenario {
			case "stale":
				gen = 1
			case "released":
				e.exec(t, "UPDATE runs SET claim_released_at=now() WHERE id=$1", id)
			case "stop":
				e.exec(t, "UPDATE runs SET stop_kind='cancelled' WHERE id=$1", id)
			case "cancelled":
				e.exec(t, "UPDATE runs SET status='cancelled' WHERE id=$1", id)
			case "newer outcome":
				e.exec(t, "UPDATE runs SET status='awaiting_input' WHERE id=$1", id)
			default:
				// Each forbidden kind must satisfy runs_kind_shape so refusal
				// reaches the park writer instead of failing fixture setup.
				target := e.fpSeedRunning(t, w, 1)
				e.exec(t, `UPDATE runs SET kind=$2, issue_iid=NULL, branch=NULL,
					repo_id=CASE WHEN $2='cross_check' THEN repo_id ELSE NULL END,
					target_run_id=CASE WHEN $2 IN ('judge','cross_check') THEN $3::uuid ELSE NULL END,
					job_type=CASE WHEN $2='job' THEN 'research' ELSE NULL END,
					report_only=($2='cross_check'), budget_wall_seconds=3600 WHERE id=$1`, id, scenario, target)
			}
			before, err := e.q.GetRunByID(e.ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			req := retainedReport(gen)
			req.MrIID = i64Ptr(42)
			_, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, id, req)
			var wantErr error
			if scenario == "stale" || scenario == "released" {
				wantErr = ErrStaleClaim
			}
			if applied || !errors.Is(err, wantErr) {
				t.Fatalf("refused park applied=%v err=%v, want applied=false err=%v", applied, err, wantErr)
			}
			after, err := e.q.GetRunByID(e.ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			a, _ := json.Marshal(before)
			b, _ := json.Marshal(after)
			if string(a) != string(b) {
				t.Fatal("refused park changed run")
			}
			if state, _ := e.fpHoldEvidence(t, hold); state != "open" {
				t.Fatal("refused park released custody")
			}
		})
	}
}

func TestRetainedParkConcurrentDuplicateLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.diskParkService(t, 3)
	svc.p.RunRetainedRecoveryMaxParks = 3
	w := e.seedWorker(t, nil)
	id := e.fpSeedRunning(t, w, 1)
	// Two independent reports; each uses the fixture context deadline and neither
	// failure blocks its sibling. WaitGroup drains both before inspecting the row.
	ctx, cancel := context.WithTimeout(e.ctx, 10*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	results := make(chan bool, 2)
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, ok, err := svc.SetState(ctx, store.Worker{ID: w}, id, retainedReport(1))
			results <- ok
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	applied := 0
	for ok := range results {
		if ok {
			applied++
		}
	}
	for err := range errs {
		if err != nil && !errors.Is(err, ErrStaleClaim) {
			t.Fatal(err)
		}
	}
	r, err := e.q.GetRunByID(e.ctx, id)
	if err != nil || applied != 1 || r.RecoveryWaitCount != 1 || r.RetainedParkCount != 1 {
		t.Fatalf("duplicate applied=%d count=%d err=%v", applied, r.RecoveryWaitCount, err)
	}
}

func TestRetainedParkRequiresPositiveGeneration(t *testing.T) {
	svc := New(&fakeStore{}, newBox(t), testParams())
	for _, gen := range []*int64{nil, i64Ptr(0), i64Ptr(-1)} {
		_, _, err := svc.parkRetainedRecovery(t.Context(), store.Worker{}, store.Run{}, StateRequest{ClaimGeneration: gen}, pgtype.Text{})
		if !errors.Is(err, ErrInvalidState) {
			t.Fatalf("generation=%v err=%v", gen, err)
		}
	}
}

func TestRetainedParkCheckpointDurabilityLiveDB(t *testing.T) {
	for _, reconciled := range []bool{false, true} {
		for _, value := range []struct {
			name     string
			reported *bool
		}{{"absent", nil}, {"false", boolPtr(false)}, {"true", boolPtr(true)}} {
			t.Run(fmt.Sprintf("reconciled=%v/%s", reconciled, value.name), func(t *testing.T) {
				e := setupInterlockLiveDB(t)
				svc := e.diskParkService(t, 3)
				w := e.seedWorker(t, nil)
				id := e.fpSeedRunning(t, w, 1)
				// An absent report must clear an older park's display value.
				e.exec(t, "UPDATE runs SET checkpoint_contains_latest=true WHERE id=$1", id)
				if reconciled {
					var input int64
					if err := e.pool.QueryRow(e.ctx, "INSERT INTO run_user_inputs(run_id,kind,body) VALUES($1,'approve_plan','') RETURNING id", id).Scan(&input); err != nil {
						t.Fatal(err)
					}
					e.exec(t, `INSERT INTO run_milestone_reconciliations(id,run_id,owner_id,gate_revision,presentation_id,
						gate_digest,reset_state_digest,worker_id,claim_generation,reason,consent,before_state,after_state,
						applied_input_id,stop_cutoff)
						VALUES($1,$2,$3,1,$4,$5,'fixture',$6,1,'fixture','{}','{}','{}',$7,0)`,
						uuid.New(), id, e.userID, uuid.New(), []byte{1}, w, input)
				}
				req := retainedReport(1)
				req.CheckpointContainsLatest = value.reported
				run, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, id, req)
				if err != nil || !applied || !run.ClaimReleasedAt.Valid {
					t.Fatalf("park applied=%v err=%v", applied, err)
				}
				got := run.CheckpointContainsLatest
				if value.reported == nil {
					if got.Valid {
						t.Fatal("flagless retained park kept stale checkpoint evidence")
					}
				} else if !got.Valid || got.Bool != *value.reported {
					t.Fatalf("checkpoint durability=%+v want %v", got, *value.reported)
				}
			})
		}
	}
}
