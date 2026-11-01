package handler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func iterationOwnerHandler(t *testing.T, f *iterationRouteFixture) *Handler {
	t.Helper()
	svc := workersvc.New(f.q, newHandlerTestBox(t), workersvc.Params{
		RunTimeout: time.Hour, RunMaxIterations: 5, CustodyHoldLimit: defaultCustodyLimit(),
	})
	svc.SetTxBeginner(f.pool)
	svc.SetHealthSettings(wallParkExtendCapFake{cap: 7200})
	return &Handler{q: f.q, wsvc: svc, cfg: config.Config{RunTimeout: time.Hour}}
}

func resumeIteration(t *testing.T, f *iterationRouteFixture, id uuid.UUID) {
	t.Helper()
	h := iterationOwnerHandler(t, f)
	rec := httptest.NewRecorder()
	h.ResumeRunNow(rec, inputReq(store.User{ID: f.user}, id, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("owner resume=%d %s", rec.Code, rec.Body.String())
	}
	r, err := f.q.GetRunByID(context.Background(), id)
	if err != nil || r.Status != "queued" || r.HoldReason.Valid || r.HoldCapturedHead.Valid ||
		!r.ClaimReleasedAt.Valid || !r.WorkerID.Valid || uuid.UUID(r.WorkerID.Bytes) != f.worker ||
		r.SessionID.String != "session-kept" || r.ClaimGeneration != 4 || r.ReleasedWorkerID.Valid || r.ReleasedWorkerNonce.Valid {
		t.Fatalf("iteration resume did not fence old flight and keep affinity/session: %+v err=%v", r, err)
	}
}

func TestIterationResumeWallBudgetLiveDB(t *testing.T) {
	f := iterationRoutes(t)
	h := iterationOwnerHandler(t, f)
	ctx := context.Background()
	for _, exhausted := range []bool{false, true} {
		t.Run(fmt.Sprintf("exhausted=%v", exhausted), func(t *testing.T) {
			id := f.run(t, "prompt")
			assertIterationAck(t, f.call(id, "iteration-park", iterationParkBody, f.worker), 200, "")
			f.exec(t, "UPDATE runs SET started_at=now()-interval '2 hours',status_since=now()-interval '30 minutes',budget_paused_seconds=13 WHERE id=$1", id)
			if !exhausted {
				f.exec(t, "UPDATE runs SET budget_wall_seconds=10800 WHERE id=$1", id)
			}
			f.exec(t, "UPDATE runs SET codex_cap_hash=$2,codex_claim_epoch=7 WHERE id=$1", id, []byte("iteration-resume-capability-hash"))
			before, err := f.q.GetRunByID(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			snapshot := f.snapshot(t, id)
			rec := httptest.NewRecorder()
			h.ResumeRunNow(rec, inputReq(store.User{ID: f.user}, id, ""))
			if exhausted {
				if rec.Code != 409 || !strings.Contains(rec.Body.String(), "extend") || !strings.Contains(rec.Body.String(), "out of time") {
					t.Fatalf("spent wall resume=%d %s", rec.Code, rec.Body.String())
				}
				if f.snapshot(t, id) != snapshot {
					t.Fatal("refused resume changed state")
				}
				// Extend adds time and leaves this iteration hold paused and unreleased.
				rec = httptest.NewRecorder()
				h.CreateRunInput(rec, inputReq(store.User{ID: f.user}, id, `{"kind":"extend","body":"7200"}`))
				if rec.Code >= 300 {
					t.Fatalf("extend=%d %s", rec.Code, rec.Body.String())
				}
				extended, err := f.q.GetRunByID(ctx, id)
				if err != nil || extended.Status != "paused" || extended.HoldReason.String != "iteration_budget_exhausted" ||
					extended.ClaimReleasedAt.Valid || extended.BudgetMaxIterations != before.BudgetMaxIterations ||
					extended.BudgetExtensionSeconds != before.BudgetExtensionSeconds+7200 ||
					extended.BudgetPausedSeconds != before.BudgetPausedSeconds {
					t.Fatalf("extend auto-resumed or changed allowance/accounting: %+v %v", extended, err)
				}
				resumeIteration(t, f, id)
			} else if rec.Code != 200 || strings.Contains(rec.Body.String(), "out of time") {
				t.Fatalf("remaining wall resume=%d %s", rec.Code, rec.Body.String())
			}
			after, err := f.q.GetRunByID(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if after.BudgetPausedSeconds < 1812 || int64(after.BudgetPausedSeconds) > int64(before.BudgetPausedSeconds)+int64(time.Since(before.StatusSince.Time).Seconds())+1 ||
				after.StartedAt != before.StartedAt || after.BudgetWallSeconds != before.BudgetWallSeconds ||
				after.BudgetFinalizeSeconds != before.BudgetFinalizeSeconds || after.BudgetMaxIterations != before.BudgetMaxIterations ||
				after.IterationCount != before.IterationCount || after.RequeueCount != before.RequeueCount ||
				after.CompletionAttempts != before.CompletionAttempts || after.CodexCapHash != nil ||
				after.CodexClaimEpoch != before.CodexClaimEpoch+1 {
				t.Fatalf("resume did not bank hold time without resetting budgets/counters: %+v", after)
			}
		})
	}
}

func TestIterationResumeReleaseFenceIsHoldScopedLiveDB(t *testing.T) {
	f := iterationRoutes(t)
	ctx := context.Background()
	for _, reason := range []string{"", "budget_exhausted", "weekly_reserve", "provider_unavailable", "iteration_budget_exhausted"} {
		t.Run("reason="+reason, func(t *testing.T) {
			id := f.run(t, "prompt")
			f.exec(t, "UPDATE runs SET status='paused',hold_reason=NULLIF($2,'') WHERE id=$1", id, reason)
			_, err := f.q.ResumePausedRun(ctx, store.ResumePausedRunParams{ID: id, UserID: f.user, GlobalTimeoutSeconds: 3600})
			if err != nil {
				t.Fatal(err)
			}
			r, err := f.q.GetRunByID(ctx, id)
			if err != nil || r.ClaimReleasedAt.Valid != (reason == "iteration_budget_exhausted") ||
				r.WorkerID.Bytes != f.worker || r.ReleasedWorkerID.Valid || r.ReleasedWorkerNonce.Valid {
				t.Fatalf("release fence leaked to another hold: %+v err=%v", r, err)
			}
		})
	}
}

func TestIterationHoldStopRefusedLiveDB(t *testing.T) {
	f := iterationRoutes(t)
	h := iterationOwnerHandler(t, f)
	for _, kind := range []string{"issue", "mr_rework", "ci_fix", "task", "prompt", "self_improve"} {
		for _, milestones := range []string{"none", "zero", "completed"} {
			t.Run(fmt.Sprintf("%s/milestones=%s", kind, milestones), func(t *testing.T) {
				id := f.run(t, kind)
				t.Cleanup(func() { f.exec(t, "UPDATE runs SET status='cancelled',finished_at=now() WHERE id=$1", id) })
				assertIterationAck(t, f.call(id, "iteration-park", iterationParkBody, f.worker), 200, "")
				if milestones != "none" {
					f.exec(t, `UPDATE runs SET milestones_frozen='[{"id":"m1","title":"one"},{"id":"m2","title":"two"}]',
 milestones_completed='[]' WHERE id=$1`, id)
					if milestones == "completed" {
						f.exec(t, `UPDATE runs SET milestones_completed='["m1"]' WHERE id=$1`, id)
					}
				}
				before := f.snapshot(t, id)
				_, err := h.wsvc.SubmitInput(context.Background(), f.user, id, "stop", "", nil)
				if err == nil || !strings.Contains(err.Error(), "iteration hold") {
					t.Fatalf("service Stop=%v", err)
				}
				rec := httptest.NewRecorder()
				h.CreateRunInput(rec, inputReq(store.User{ID: f.user}, id, `{"kind":"stop"}`))
				if rec.Code != 409 || !strings.Contains(rec.Body.String(), "Resume") || !strings.Contains(rec.Body.String(), "Cancel") {
					t.Fatalf("HTTP Stop=%d %s", rec.Code, rec.Body.String())
				}
				if f.snapshot(t, id) != before {
					t.Fatal("Stop wrote input, scope, finalize allowance or release")
				}
				r, err := f.q.GetRunByID(context.Background(), id)
				if err != nil {
					t.Fatal(err)
				}
				if dto := runToDTO(r, "", time.Hour, 7200, 0, time.Now(), 0); dto.CanStopAtWall {
					t.Fatal("iteration hold advertised wall Stop")
				}
			})
		}
	}
}

// iterationStopBeginner runs its hook once before opening the owner's transaction;
// the worker park uses the fixture's separate service and real pool transaction.
type iterationStopBeginner struct {
	workersvc.TxBeginner
	beforeBegin func()
}

func (b *iterationStopBeginner) Begin(ctx context.Context) (pgx.Tx, error) {
	if hook := b.beforeBegin; hook != nil {
		b.beforeBegin = nil
		hook()
	}
	return b.TxBeginner.Begin(ctx)
}

func TestIterationHoldStopLockedRecheckLiveDB(t *testing.T) {
	for _, park := range []bool{true, false} {
		t.Run(fmt.Sprintf("parkBeforeBegin=%v", park), func(t *testing.T) {
			f := iterationRoutes(t)
			h := iterationOwnerHandler(t, f)
			ctx := context.Background()
			id := f.run(t, "issue")
			f.exec(t, `UPDATE runs SET milestones_frozen='[{"id":"m1","title":"one"},{"id":"m2","title":"two"}]',
 milestones_completed='["m1"]' WHERE id=$1`, id)
			inputCounts := func() (scope, stop int) {
				t.Helper()
				if err := f.pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE kind='scope'),
 count(*) FILTER (WHERE kind='stop') FROM run_user_inputs WHERE run_id=$1`, id).Scan(&scope, &stop); err != nil {
					t.Fatal(err)
				}
				return scope, stop
			}
			assertNoScopeOrStop := func() {
				t.Helper()
				r, err := f.q.GetRunByID(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				scope, stop := inputCounts()
				if r.ScopeCeiling.Valid || scope != 0 || stop != 0 {
					t.Fatalf("unexpected ceiling/inputs: ceiling=%+v scope=%d stop=%d", r.ScopeCeiling, scope, stop)
				}
			}
			assertNoScopeOrStop()
			var parkedSnapshot string
			begins := 0
			h.wsvc.SetTxBeginner(&iterationStopBeginner{TxBeginner: f.pool, beforeBegin: func() {
				begins++
				r, err := f.q.GetRunByID(ctx, id)
				if err != nil || r.Status != "running" || r.HoldReason.Valid {
					t.Fatalf("Stop did not reach Begin from a running run: %+v err=%v", r, err)
				}
				if park {
					assertIterationAck(t, f.call(id, "iteration-park", iterationParkBody, f.worker), http.StatusOK, "")
					r, err = f.q.GetRunByID(ctx, id)
					if err != nil || r.Status != "paused" || r.HoldReason.String != "iteration_budget_exhausted" {
						t.Fatalf("worker park did not commit: %+v err=%v", r, err)
					}
					assertNoScopeOrStop()
					parkedSnapshot = f.snapshot(t, id)
				}
			}})
			rec := httptest.NewRecorder()
			h.CreateRunInput(rec, inputReq(store.User{ID: f.user}, id, `{"kind":"stop"}`))
			if begins != 1 {
				t.Fatalf("Stop bypassed the transaction interleaving: begins=%d", begins)
			}
			if park {
				if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "Resume") || !strings.Contains(rec.Body.String(), "Cancel") {
					t.Fatalf("HTTP Stop=%d %s", rec.Code, rec.Body.String())
				}
				assertNoScopeOrStop()
				if f.snapshot(t, id) != parkedSnapshot {
					t.Fatal("refused Stop changed the committed iteration park")
				}
				return
			}
			if rec.Code < 200 || rec.Code >= 300 {
				t.Fatalf("running HTTP Stop=%d %s", rec.Code, rec.Body.String())
			}
			r, err := f.q.GetRunByID(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			scope, stop := inputCounts()
			if r.Status != "running" || r.HoldReason.Valid || !r.ScopeCeiling.Valid || r.ScopeCeiling.Int32 != 1 ||
				r.StopKind.Valid || scope != 1 || stop != 0 {
				t.Fatalf("running Stop lost scope authority: run=%+v scope=%d stop=%d", r, scope, stop)
			}
		})
	}
}

// TestIterationResumeUpdateWallGuardLiveDB blocks ResumePausedRun on its parent
// row lock after candidate selection, then commits an exhausted wall. This exercises
// the UPDATE predicate independently of the candidate predicate.
func TestIterationResumeUpdateWallGuardLiveDB(t *testing.T) {
	f := iterationRoutes(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	id := f.run(t, "prompt")
	assertIterationAck(t, f.call(id, "iteration-park", iterationParkBody, f.worker), 200, "")
	blocker, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Rollback(context.Background()) }()
	if _, err := blocker.Exec(ctx, "UPDATE runs SET budget_wall_seconds=1,budget_extension_seconds=0,budget_finalize_seconds=0,budget_paused_seconds=0,started_at=status_since-interval '1 hour' WHERE id=$1", id); err != nil {
		t.Fatal(err)
	}
	conn, err := f.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	var pid int32
	if err := conn.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := store.New(conn).ResumePausedRun(ctx, store.ResumePausedRunParams{ID: id, UserID: f.user, GlobalTimeoutSeconds: 3600})
		result <- err
	}()
	// At most ten seconds, one lock observation every 10ms. On every exit,
	// cancel the query and join it before releasing its connection.
	defer func() { cancel(); <-result }()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var blocked bool
		if err := f.pool.QueryRow(ctx, "SELECT cardinality(pg_blocking_pids($1))>0", pid).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("resume did not reach its parent row lock")
		case <-ticker.C:
		}
	}
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	err = <-result
	result <- err
	if err != pgx.ErrNoRows {
		t.Fatalf("UPDATE admitted spent wall after candidate selection: %v", err)
	}
	r, err := f.q.GetRunByID(ctx, id)
	if err != nil || r.Status != "paused" || r.ClaimReleasedAt.Valid || r.HoldReason.String != "iteration_budget_exhausted" {
		t.Fatalf("refused UPDATE changed flight: %+v %v", r, err)
	}
}

func TestIterationResumeClaimSnapshotLifecycleLiveDB(t *testing.T) {
	f := iterationRoutes(t)
	ctx := context.Background()
	id := f.run(t, "prompt")
	assertIterationAck(t, f.call(id, "iteration-park", iterationParkBody, f.worker), 200, "")
	f.exec(t, "UPDATE workers SET snapshot_register_nonce='iteration-resume',snapshot_epoch=1 WHERE id=$1", f.worker)
	f.exec(t, `INSERT INTO worker_active_runs(worker_id,run_id,claim_generation,phase,terminal_pending,snapshot_epoch,reported_at)
 VALUES($1,$2,4,'running',false,1,now())`, f.worker, id)
	hold := uuid.New()
	f.exec(t, `INSERT INTO recovery_custody_holds(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id)
 VALUES($1,$2,$3,$4,4,'open',$5,'iteration-resume',$5,$4)`, hold, f.user, f.repo, id, f.worker)
	custody := func() string {
		var row string
		if err := f.pool.QueryRow(ctx, "SELECT row_to_json(h)::text FROM recovery_custody_holds h WHERE id=$1", hold).Scan(&row); err != nil {
			t.Fatal(err)
		}
		return row
	}
	beforeCustody := custody()
	resumeIteration(t, f, id)
	if custody() != beforeCustody {
		t.Fatal("owner resume changed custody")
	}
	params := store.ClaimRunParams{
		WorkerID: pgconv.UUID(f.worker), UserID: f.user, CapabilityAware: true,
		WorkerCaps: []string{}, WorkerProtocolCaps: []string{}, DockerRepoAllowlist: []uuid.UUID{},
		CustodyHoldLimit: 10, SnapshotFreshCutoff: pgconv.Time(time.Now().Add(-time.Minute)),
		RequestActiveIds: []uuid.UUID{id}, RequestActiveGens: []int64{4},
		WorkerIdentity: "iteration-resume", BackgroundGraceCutoff: pgconv.Time(time.Now()),
	}
	before := f.snapshot(t, id)
	if _, err := f.q.ClaimRun(ctx, params); err != pgx.ErrNoRows {
		t.Fatalf("actually active request flight did not block claim: %v", err)
	}
	if f.snapshot(t, id) != before {
		t.Fatal("excluded claim changed run")
	}
	// Persisted old snapshot remains present but cannot block a released flight.
	params.RequestActiveIds, params.RequestActiveGens = []uuid.UUID{}, []int64{}
	claimed, err := f.q.ClaimRun(ctx, params)
	if err != nil || claimed.ID != id || claimed.ClaimGeneration != 5 || claimed.ClaimReleasedAt.Valid ||
		claimed.SessionID.String != "session-kept" || claimed.BudgetMaxIterations.Int32 != 5 {
		t.Fatalf("continued claim=%+v err=%v", claimed, err)
	}
	rec := f.call(id, "state", `{"status":"running","claim_generation":5}`, f.worker)
	if rec.Code != 200 {
		t.Fatalf("fresh flight running=%d %s", rec.Code, rec.Body.String())
	}
	rec = f.call(id, "iteration-park", strings.Replace(iterationParkBody, `"claim_generation":4`, `"claim_generation":5`, 1), f.worker)
	if rec.Code != 200 {
		t.Fatalf("fresh finite invocation repeat park=%d %s", rec.Code, rec.Body.String())
	}
	r, err := f.q.GetRunByID(ctx, id)
	if err != nil || r.Status != "paused" || r.HoldReason.String != "iteration_budget_exhausted" ||
		r.BudgetMaxIterations.Int32 != 5 || r.SessionID.String != "session-kept" || r.ClaimGeneration != 5 {
		t.Fatalf("repeat lifecycle=%+v %v", r, err)
	}
}
