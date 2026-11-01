package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

const iterationParkBody = `{"claim_generation":4,"iteration_count":5,"max_iterations":5,"head":"","published":false}`

type iterationRouteFixture struct {
	*jobRouteEnv
	q            *store.Queries
	repo, parent uuid.UUID
	seq          int64
}

func iterationRoutes(t *testing.T) *iterationRouteFixture {
	t.Helper()
	e := setupJobRouteLiveDB(t)
	f := &iterationRouteFixture{jobRouteEnv: e, q: store.New(e.pool), repo: uuid.New(), parent: uuid.New()}
	conn := uuid.New()
	e.exec(t, `INSERT INTO forge_connections(id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext)
 VALUES($1,$2,'gitlab','https://forge.e2e','bot',1,$3)`, conn, e.user, []byte{1})
	e.exec(t, `INSERT INTO repos(id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch,enabled)
 VALUES($1,$2,1,$3,'https://forge.e2e/g/r','main',true)`, f.repo, conn, "iterations-"+f.repo.String())
	e.exec(t, `INSERT INTO runs(id,user_id,repo_id,issue_iid,issue_title,issue_description,status)
 VALUES($1,$2,$3,0,'parent','d','completed')`, f.parent, e.user, f.repo)
	t.Cleanup(func() {
		// Custody deliberately has restrictive live pointers and immutable provenance.
		e.exec(t, "DELETE FROM recovery_custody_holds WHERE user_id=$1", e.user)
		e.exec(t, "DELETE FROM users WHERE id=$1", e.user)
	})
	f.install(t, workersvc.Params{RunMaxIterations: 5, RunTimeout: time.Hour}, nil)
	return f
}

func (f *iterationRouteFixture) install(t *testing.T, p workersvc.Params, hook func()) {
	t.Helper()
	svc := workersvc.New(f.q, newHandlerTestBox(t), p)
	svc.SetTxBeginner(pauseTestBeginner{pool: f.pool, beforeLock: hook})
	h := &Handler{q: f.q, wsvc: svc}
	f.router = h.WorkerRoutes(mw.NewLimiter(10000, time.Minute, nil))
}

func (f *iterationRouteFixture) run(t *testing.T, kind string, profiles ...uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	var profile any
	if len(profiles) > 0 {
		profile = profiles[0]
	}
	f.seq++
	f.exec(t, `INSERT INTO runs(id,user_id,repo_id,kind,issue_iid,issue_title,issue_description,status,worker_id,
 claim_generation,started_at,budget_wall_seconds,budget_max_iterations,session_id,branch,pipeline_id,pipeline_ref,mr_iid,target_run_id,
 job_type,report_only,budget_paused_seconds,budget_extension_seconds,budget_finalize_seconds,iteration_count,egress_profile_id)
 VALUES($1,$2,CASE WHEN $4 IN ('chat','judge','job') THEN NULL ELSE $3::uuid END,$4,
 CASE WHEN $4 IN ('issue','self_improve') THEN $5::bigint END,'t','d','running',$6,4,now(),3600,5,'session-kept',
 CASE WHEN $4 IN ('task','mr_rework') THEN $7::text END,
 CASE WHEN $4='ci_fix' THEN $5::bigint END,CASE WHEN $4 IN ('ci_fix','mr_rework') THEN $7::text END,
 CASE WHEN $4='mr_rework' THEN $5::bigint END,
 CASE WHEN $4 IN ('mr_rework','judge','cross_check') THEN $8::uuid END,
 CASE WHEN $4='job' THEN 'research' END,$4='cross_check',13,17,19,99,$9::uuid)`,
		id, f.user, f.repo, kind, f.seq, f.worker, "branch-"+id.String(), f.parent, profile)
	return id
}

func (f *iterationRouteFixture) call(id uuid.UUID, endpoint, body string, worker uuid.UUID) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/worker/runs/"+id.String()+"/"+endpoint, strings.NewReader(body))
	if worker != uuid.Nil {
		req.Header.Set("Authorization", "Bearer "+f.tokens[worker])
	}
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}

func (f *iterationRouteFixture) snapshot(t *testing.T, id uuid.UUID) string {
	t.Helper()
	var snapshot string
	err := f.pool.QueryRow(context.Background(), `SELECT jsonb_build_object(
 'run',to_jsonb(r),
 'inputs',(SELECT jsonb_agg(i ORDER BY id) FROM run_user_inputs i WHERE i.run_id=r.id),
 'attempts',(SELECT jsonb_agg(a ORDER BY id) FROM run_completion_attempts a WHERE a.run_id=r.id),
 'custody',(SELECT jsonb_agg(h ORDER BY id) FROM recovery_custody_holds h WHERE h.run_id=r.id))::text
 FROM runs r WHERE id=$1`, id).Scan(&snapshot)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func assertIterationAck(t *testing.T, rec *httptest.ResponseRecorder, code int, reason string) {
	t.Helper()
	var ack struct {
		Applied         bool   `json:"applied"`
		Status          string `json:"status"`
		ClaimGeneration int64  `json:"claim_generation"`
		HoldReason      string `json:"hold_reason"`
		Reason          string `json:"reason"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &ack); err != nil {
		t.Fatal(err)
	}
	if rec.Code != code || ack.Applied != (code == 200) || ack.Reason != reason || len(rec.Body.Bytes()) > 512 {
		t.Fatalf("ack=%d %s want code=%d reason=%s", rec.Code, rec.Body.String(), code, reason)
	}
	if code == 200 && (ack.Status != "paused" || ack.ClaimGeneration != 4 || ack.HoldReason != "iteration_budget_exhausted") {
		t.Fatalf("incorrect success identity: %s", rec.Body.String())
	}
}

func TestWorkerIterationParkLiveDB(t *testing.T) {
	f := iterationRoutes(t)
	ctx := context.Background()
	for _, kind := range []string{"issue", "mr_rework", "ci_fix", "task", "prompt", "self_improve"} {
		t.Run("eligible/"+kind, func(t *testing.T) {
			id := f.run(t, kind)
			before, err := f.q.GetRunByID(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			hold := uuid.New()
			f.exec(t, `INSERT INTO recovery_custody_holds(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id)
   VALUES($1,$2,$3,$4,4,'open',$5,'iteration-fixture',$5,$4)`, hold, f.user, f.repo, id, f.worker)
			var custody string
			if err := f.pool.QueryRow(ctx, "SELECT row_to_json(h)::text FROM recovery_custody_holds h WHERE id=$1", hold).Scan(&custody); err != nil {
				t.Fatal(err)
			}
			f.exec(t, `UPDATE runs SET pause_mode='milestone',pause_requested_at=now(),pause_after_count=1 WHERE id=$1`, id)
			for _, body := range []string{"now", "milestone", "wall", "reserve"} {
				f.exec(t, "INSERT INTO run_user_inputs(run_id,kind,body) VALUES($1,'pause',$2)", id, body)
			}
			rec := f.call(id, "iteration-park", strings.Replace(iterationParkBody, "}", `,"checkpoint_contains_latest":false}`, 1), f.worker)
			assertIterationAck(t, rec, 200, "")
			after, err := f.q.GetRunByID(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if after.CompletionAttempts != 0 || after.PauseMode.Valid || after.PauseRequestedAt.Valid || after.PauseAfterCount.Valid ||
				!after.CheckpointContainsLatest.Valid || after.CheckpointContainsLatest.Bool || after.ClaimReleasedAt.Valid {
				t.Fatalf("park fields: %+v", after)
			}
			// Compare all fields outside the transition's documented write set.
			before.Status = after.Status
			before.StatusSince = after.StatusSince
			before.UpdatedAt = after.UpdatedAt
			before.HoldReason = after.HoldReason
			before.CheckpointContainsLatest = after.CheckpointContainsLatest
			if !reflect.DeepEqual(before, after) {
				t.Fatal("park changed fields outside its write set (budgets/session/attempts/claim)")
			}
			var pending int
			if err := f.pool.QueryRow(ctx, "SELECT count(*) FROM run_user_inputs WHERE run_id=$1 AND (applied_at IS NULL OR consumed_at IS NULL OR consumed_claim_generation<>4)", id).Scan(&pending); err != nil || pending != 0 {
				t.Fatalf("unsettled pauses=%d err=%v", pending, err)
			}
			var got string
			if err := f.pool.QueryRow(ctx, "SELECT row_to_json(h)::text FROM recovery_custody_holds h WHERE id=$1", hold).Scan(&got); err != nil || got != custody {
				t.Fatalf("custody changed: %v", err)
			}
		})
	}
	for _, tc := range []struct{ name, kind, update, reason string }{
		{"chat", "chat", "", "ineligible_lane"}, {"judge", "judge", "", "ineligible_lane"},
		{"job", "job", "", "ineligible_lane"}, {"cross_check", "cross_check", "", "ineligible_lane"},
		{"interactive", "prompt", "interactive=true", "ineligible_lane"},
		{"review_target", "prompt", fmt.Sprintf("review_target_run_id='%s'", f.parent), "ineligible_lane"},
		{"attempt", "prompt", "completion_attempts=1", "prior_attempt"},
		{"stale", "prompt", "claim_generation=5", "stale_claim"},
		{"released", "prompt", "claim_released_at=now()", "released_claim"},
		{"same_worker_reclaimed", "prompt", "claim_generation=6", "stale_claim"},
		{"cancelled", "prompt", "status='cancelled'", "not_running"},
		{"queued", "prompt", "status='queued'", "not_running"},
		{"cap", "prompt", "budget_max_iterations=7", "allowance_mismatch"},
		{"stop", "prompt", "stop_kind='stopped'", "pending_control"},
		{"switch", "prompt", "credential_switch_requested_at=now(),credential_switch_generation=4", "pending_control"},
	} {
		t.Run("refusal/"+tc.name, func(t *testing.T) {
			id := f.run(t, tc.kind)
			if tc.update != "" {
				f.exec(t, "UPDATE runs SET "+tc.update+" WHERE id=$1", id)
			}
			if tc.name == "attempt" {
				f.exec(t, `INSERT INTO run_completion_attempts(run_id,unmet) VALUES($1,'[]')`, id)
			}
			before := f.snapshot(t, id)
			assertIterationAck(t, f.call(id, "iteration-park", iterationParkBody, f.worker), 409, tc.reason)
			if f.snapshot(t, id) != before {
				t.Fatal("refusal changed persistent state")
			}
		})
	}
	t.Run("isolated", func(t *testing.T) {
		profile := uuid.New()
		f.exec(t, "INSERT INTO egress_profiles(id,name,hosts) VALUES($1,$2,ARRAY['example.com'])", profile, "iteration-"+profile.String())
		id := f.run(t, "prompt", profile)
		t.Cleanup(func() {
			f.exec(t, "DELETE FROM runs WHERE id=$1", id)
			f.exec(t, "DELETE FROM egress_profiles WHERE id=$1", profile)
		})
		before := f.snapshot(t, id)
		assertIterationAck(t, f.call(id, "iteration-park", iterationParkBody, f.worker), 409, "ineligible_lane")
		if f.snapshot(t, id) != before {
			t.Fatal("isolated refusal wrote state")
		}
	})
	t.Run("pending_input", func(t *testing.T) {
		for _, kind := range []string{"stop", "cancel"} {
			id := f.run(t, "prompt")
			f.exec(t, "INSERT INTO run_user_inputs(run_id,kind) VALUES($1,$2)", id, kind)
			before := f.snapshot(t, id)
			assertIterationAck(t, f.call(id, "iteration-park", iterationParkBody, f.worker), 409, "pending_control")
			if f.snapshot(t, id) != before {
				t.Fatal("pending-control refusal wrote state")
			}
		}
	})
	t.Run("authentication_and_ownership", func(t *testing.T) {
		id := f.run(t, "prompt")
		before := f.snapshot(t, id)
		foreign := f.seedWorker(t, f.user)
		for _, tc := range []struct {
			id, worker uuid.UUID
			code       int
		}{{id, uuid.Nil, 401}, {id, foreign, 404}, {uuid.New(), f.worker, 404}} {
			rec := f.call(tc.id, "iteration-park", iterationParkBody, tc.worker)
			if rec.Code != tc.code {
				t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
			}
		}
		if f.snapshot(t, id) != before {
			t.Fatal("ownership refusal wrote state")
		}
	})
	t.Run("malformed_unchanged", func(t *testing.T) {
		id := f.run(t, "prompt")
		before := f.snapshot(t, id)
		for _, body := range []string{
			"{}", "null", strings.Replace(iterationParkBody, `"head":"",`, "", 1),
			strings.Replace(iterationParkBody, `"published":false`, `"published":null`, 1),
			strings.Replace(iterationParkBody, `"claim_generation":4`, `"claim_generation":null`, 1),
			strings.Replace(iterationParkBody, `"iteration_count":5`, `"iteration_count":5.5`, 1),
			strings.Replace(iterationParkBody, `"iteration_count":5`, `"iteration_count":7`, 1),
			strings.Replace(iterationParkBody, `"iteration_count":5`, `"iteration_count":4`, 1),
			strings.Replace(iterationParkBody, `"max_iterations":5`, `"max_iterations":2147483648`, 1),
			strings.Replace(iterationParkBody, "}", `,"unknown":true}`, 1),
			iterationParkBody + strings.Repeat(" ", 16<<10),
		} {
			rec := f.call(id, "iteration-park", body, f.worker)
			if rec.Code != 400 {
				t.Fatalf("malformed=%d %s", rec.Code, rec.Body.String())
			}
			if f.snapshot(t, id) != before {
				t.Fatal("malformed evidence wrote state")
			}
		}
	})
	t.Run("changed_default_before_first_park", func(t *testing.T) {
		id := f.run(t, "prompt")
		f.exec(t, "UPDATE runs SET budget_max_iterations=NULL WHERE id=$1", id)
		f.install(t, workersvc.Params{RunMaxIterations: 8, RunTimeout: time.Hour}, nil)
		defer f.install(t, workersvc.Params{RunMaxIterations: 5, RunTimeout: time.Hour}, nil)
		before := f.snapshot(t, id)
		assertIterationAck(t, f.call(id, "iteration-park", iterationParkBody, f.worker), 409, "allowance_mismatch")
		if f.snapshot(t, id) != before {
			t.Fatal("changed-default refusal persisted an allowance or changed run state")
		}
	})
	t.Run("replay", func(t *testing.T) {
		id := f.run(t, "prompt")
		f.exec(t, "UPDATE runs SET budget_max_iterations=NULL WHERE id=$1", id)
		assertIterationAck(t, f.call(id, "iteration-park", iterationParkBody, f.worker), 200, "")
		before := f.snapshot(t, id)
		// A changed configured fallback must not break a lost-ACK duplicate.
		f.install(t, workersvc.Params{RunMaxIterations: 8, RunTimeout: time.Hour}, nil)
		t.Cleanup(func() { f.install(t, workersvc.Params{RunMaxIterations: 5, RunTimeout: time.Hour}, nil) })
		assertIterationAck(t, f.call(id, "iteration-park", iterationParkBody, f.worker), 200, "")
		if f.snapshot(t, id) != before {
			t.Fatal("duplicate changed status_since or accounting")
		}
		head := strings.Replace(iterationParkBody, `"head":""`, `"head":"first"`, 1)
		assertIterationAck(t, f.call(id, "iteration-park", head, f.worker), 200, "")
		r, err := f.q.GetRunByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if r.HoldCapturedHead.String != "first" {
			t.Fatal("missing head not added")
		}
		before = f.snapshot(t, id)
		assertIterationAck(t, f.call(id, "iteration-park", strings.Replace(head, "first", "second", 1), f.worker), 200, "")
		if f.snapshot(t, id) != before {
			t.Fatal("duplicate replaced head")
		}
		f.exec(t, "UPDATE runs SET claim_released_at=now(),hold_captured_head=NULL WHERE id=$1", id)
		before = f.snapshot(t, id)
		assertIterationAck(t, f.call(id, "iteration-park", head, f.worker), 409, "released_claim")
		if f.snapshot(t, id) != before {
			t.Fatal("released flight added head")
		}
	})
	t.Run("replay_prior_attempt", func(t *testing.T) {
		id := f.run(t, "prompt")
		assertIterationAck(t, f.call(id, "iteration-park", iterationParkBody, f.worker), 200, "")
		f.exec(t, "UPDATE runs SET completion_attempts=1 WHERE id=$1", id)
		f.exec(t, `INSERT INTO run_completion_attempts(run_id,unmet) VALUES($1,'[]')`, id)
		before := f.snapshot(t, id)
		assertIterationAck(t, f.call(id, "iteration-park", iterationParkBody, f.worker), 409, "prior_attempt")
		if f.snapshot(t, id) != before {
			t.Fatal("replay with prior history changed persistent state")
		}
	})
	t.Run("wall_before_deadline_control", func(t *testing.T) {
		id := f.run(t, "prompt")
		before := f.snapshot(t, id)
		rec := f.call(id, "wall-park", `{"claim_generation":4,"head":"","published":false}`, f.worker)
		if rec.Code != 409 {
			t.Fatalf("wall before deadline=%d %s", rec.Code, rec.Body.String())
		}
		if f.snapshot(t, id) != before {
			t.Fatal("wall refusal changed state")
		}
	})
}

// TestWorkerIterationParkRacesLiveDB puts the losing request inside Begin,
// before its worker/run locks, then lets the competing transaction finish.
// Channels establish each winner order without timing-dependent sleeps.
func TestWorkerIterationParkRacesLiveDB(t *testing.T) {
	for _, competitor := range []string{"attempt", "cancel"} {
		for _, parkWins := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/park_wins=%v", competitor, parkWins), func(t *testing.T) {
				f := iterationRoutes(t)
				id := f.run(t, "issue")
				f.exec(t, "UPDATE runs SET completion_contract_version=1,contract_revision=1 WHERE id=$1", id)
				plain := f.router
				serve := func(router http.Handler, endpoint, body string) *httptest.ResponseRecorder {
					req := httptest.NewRequest("POST", "/api/worker/runs/"+id.String()+"/"+endpoint, strings.NewReader(body))
					ctx, cancel := context.WithTimeout(req.Context(), 10*time.Second)
					defer cancel()
					req = req.WithContext(ctx)
					req.Header.Set("Authorization", "Bearer "+f.tokens[f.worker])
					rec := httptest.NewRecorder()
					router.ServeHTTP(rec, req)
					return rec
				}
				entered, release := make(chan struct{}), make(chan struct{})
				// Always release the barrier, including when a winner assertion fails.
				var once sync.Once
				unblock := func() { once.Do(func() { close(release) }) }
				defer unblock()
				wait := func() { close(entered); <-release }
				var losing func() *httptest.ResponseRecorder
				if !parkWins {
					f.install(t, workersvc.Params{RunMaxIterations: 5, RunTimeout: time.Hour}, wait)
					losing = func() *httptest.ResponseRecorder { return serve(f.router, "iteration-park", iterationParkBody) }
				} else if competitor == "attempt" {
					f.install(t, workersvc.Params{RunMaxIterations: 5, RunTimeout: time.Hour}, wait)
					losing = func() *httptest.ResponseRecorder {
						return serve(f.router, "completion/attempt", `{"claim_generation":4,"head":"head","worktree_fingerprint":"tree"}`)
					}
				} else {
					losing = func() *httptest.ResponseRecorder {
						ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
						defer cancel()
						tx, err := f.pool.Begin(ctx)
						rec := httptest.NewRecorder()
						if err != nil {
							rec.WriteHeader(500)
							return rec
						}
						defer func() { _ = tx.Rollback(ctx) }()
						wait()
						n, err := store.New(tx).CancelRunServerSide(ctx, store.CancelRunServerSideParams{ID: id, UserID: f.user})
						if err != nil || n != 1 {
							rec.WriteHeader(500)
							return rec
						}
						if err := tx.Commit(ctx); err != nil {
							rec.WriteHeader(500)
							return rec
						}
						rec.WriteHeader(200)
						return rec
					}
				}
				result := make(chan *httptest.ResponseRecorder, 1)
				go func() { result <- losing() }()
				select {
				case <-entered:
				case <-time.After(10 * time.Second):
					unblock()
					<-result
					t.Fatal("losing request never reached lock barrier")
				}
				var winningPark *httptest.ResponseRecorder
				if parkWins {
					winningPark = serve(plain, "iteration-park", iterationParkBody)
				} else if competitor == "attempt" {
					rec := serve(plain, "completion/attempt", `{"claim_generation":4,"head":"head","worktree_fingerprint":"tree"}`)
					if rec.Code != 200 {
						unblock()
						<-result
						t.Fatalf("winning attempt=%d %s", rec.Code, rec.Body.String())
					}
				} else {
					n, err := f.q.CancelRunServerSide(context.Background(), store.CancelRunServerSideParams{ID: id, UserID: f.user})
					if err != nil || n != 1 {
						unblock()
						<-result
						t.Fatalf("winning cancel=%d %v", n, err)
					}
				}
				unblock()
				rec := <-result
				if winningPark != nil {
					assertIterationAck(t, winningPark, 200, "")
				}
				if !parkWins {
					reason := "prior_attempt"
					if competitor == "cancel" {
						reason = "not_running"
					}
					assertIterationAck(t, rec, 409, reason)
				} else if competitor == "attempt" && rec.Code != 409 {
					t.Fatalf("losing attempt=%d %s", rec.Code, rec.Body.String())
				} else if competitor == "cancel" && rec.Code != 200 {
					t.Fatalf("owner cancel=%d", rec.Code)
				}
				run, err := f.q.GetRunByID(context.Background(), id)
				if err != nil {
					t.Fatal(err)
				}
				wantAttempts := int32(0)
				wantStatus := "paused"
				if competitor == "cancel" {
					wantStatus = "cancelled"
				} else if !parkWins {
					wantAttempts = 1
					wantStatus = "running"
				}
				if run.CompletionAttempts != wantAttempts || run.Status != wantStatus {
					t.Fatalf("race status=%s attempts=%d want %s/%d", run.Status, run.CompletionAttempts, wantStatus, wantAttempts)
				}
				var attempts int32
				if err := f.pool.QueryRow(context.Background(), "SELECT count(*) FROM run_completion_attempts WHERE run_id=$1", id).Scan(&attempts); err != nil || attempts != wantAttempts {
					t.Fatalf("attempt rows=%d %v", attempts, err)
				}
			})
		}
	}
}

// Recovery queries must leave an iteration hold intact even when a matching
// live snapshot reports the old phase, or a later snapshot omits the attempt.
func TestWorkerIterationParkRecoveryQueriesLiveDB(t *testing.T) {
	for _, frozen := range []bool{false, true} {
		t.Run(fmt.Sprintf("frozen=%v", frozen), func(t *testing.T) {
			f := iterationRoutes(t)
			ctx := context.Background()
			held, control := f.run(t, "prompt"), f.run(t, "prompt")
			body := strings.Replace(iterationParkBody, `"head":""`, `"head":"captured-head"`, 1)
			assertIterationAck(t, f.call(held, "iteration-park", body, f.worker), 200, "")
			ids := []uuid.UUID{held, control}
			f.exec(t, "UPDATE runs SET status_since=now()-interval '10 minutes',requeue_count=1 WHERE id=ANY($1::uuid[])", ids)
			f.exec(t, "UPDATE runs SET status='queued' WHERE id=$1", control)
			f.exec(t, "UPDATE workers SET snapshot_register_nonce='iteration-recovery',snapshot_epoch=1 WHERE id=$1", f.worker)
			for _, id := range ids {
				f.exec(t, `INSERT INTO worker_active_runs(worker_id,run_id,claim_generation,phase,terminal_pending,snapshot_epoch,reported_at)
 VALUES($1,$2,4,'running',false,1,now())`, f.worker, id)
			}
			var ledger []byte
			if err := f.pool.QueryRow(ctx, `SELECT jsonb_agg(jsonb_build_object(
 'id',id,'kind',kind,'target_run_id',target_run_id,'worker_id',worker_id,
 'claim_generation',claim_generation,'cross_check_id',NULL,'parent_lead_id',id))
 FROM runs WHERE id=ANY($1::uuid[])`, ids).Scan(&ledger); err != nil {
				t.Fatal(err)
			}
			before := f.snapshot(t, held)
			// The complete snapshot comparison below includes head, generation,
			// session, allowance, every budget counter and claim release identity.
			parked, err := f.q.GetRunByID(ctx, held)
			if err != nil {
				t.Fatal(err)
			}
			if parked.Status != "paused" || parked.HoldReason.String != "iteration_budget_exhausted" ||
				parked.HoldCapturedHead.String != "captured-head" || parked.ClaimGeneration != 4 {
				t.Fatal("fixture did not establish the iteration hold")
			}
			runQuery := func(name string, check func(*store.Queries)) {
				t.Helper()
				tx, err := f.pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = tx.Rollback(ctx) }()
				q := store.New(tx)
				if _, err := q.GetWorkerForUpdate(ctx, f.worker); err != nil {
					t.Fatal(err)
				}
				if _, err := tx.Exec(ctx, "SELECT id FROM runs WHERE id=ANY($1::uuid[]) ORDER BY id FOR UPDATE", ids); err != nil {
					t.Fatal(err)
				}
				check(q)
				if err := tx.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				if f.snapshot(t, held) != before {
					t.Fatalf("%s changed the iteration hold, generation, head or budgets", name)
				}
			}
			runQuery("readoption", func(q *store.Queries) {
				if frozen {
					rows, err := q.FrozenReadoptRunsFromSnapshot(ctx, store.FrozenReadoptRunsFromSnapshotParams{
						WorkerID: f.worker, FrozenTargets: ledger, LockedParentIds: ids,
					})
					if err != nil || len(rows) != 1 || rows[0].ID != control || rows[0].Status != "running" {
						t.Fatalf("frozen readopt=%v err=%v; want only running control", rows, err)
					}
				} else {
					rows, err := q.ReadoptRunsFromSnapshot(ctx, f.worker)
					if err != nil || len(rows) != 1 || rows[0].ID != control || rows[0].Status != "running" {
						t.Fatalf("readopt=%v err=%v; want only running control", rows, err)
					}
				}
			})
			// Make both attempts old enough for missing-snapshot recovery.
			// The held attempt is otherwise eligible for both requeue caps.
			f.exec(t, "DELETE FROM worker_active_runs WHERE worker_id=$1", f.worker)
			f.exec(t, "UPDATE workers SET snapshot_epoch=2 WHERE id=$1", f.worker)
			f.exec(t, "UPDATE runs SET status_since=now()-interval '10 minutes' WHERE id=$1", control)
			now := time.Now()
			cutoff, observed := pgconv.Time(now.Add(-time.Minute)), pgconv.Time(now)
			runQuery("missing requeue", func(q *store.Queries) {
				if frozen {
					rows, err := q.FrozenRequeueRunsMissingFromSnapshot(ctx, store.FrozenRequeueRunsMissingFromSnapshotParams{
						WorkerID: pgconv.UUID(f.worker), MissingCutoff: cutoff, MaxRequeues: 2,
						Now: observed, GlobalTimeoutSeconds: 3600, FrozenTargets: ledger, LockedParentIds: ids,
					})
					if err != nil || len(rows) != 1 || rows[0].ID != control || rows[0].Status != "queued" {
						t.Fatalf("frozen missing requeue=%v err=%v; want only queued control", rows, err)
					}
				} else {
					rows, err := q.RequeueRunsMissingFromSnapshot(ctx, store.RequeueRunsMissingFromSnapshotParams{
						WorkerID: pgconv.UUID(f.worker), MissingCutoff: cutoff, MaxRequeues: 2,
						Now: observed, GlobalTimeoutSeconds: 3600,
					})
					if err != nil || len(rows) != 1 || rows[0].ID != control || rows[0].Status != "queued" {
						t.Fatalf("missing requeue=%v err=%v; want only queued control", rows, err)
					}
				}
			})
			f.exec(t, "UPDATE runs SET status='running',status_since=now()-interval '10 minutes' WHERE id=$1", control)
			runQuery("missing exhaustion", func(q *store.Queries) {
				if frozen {
					rows, err := q.FrozenFailRunsMissingFromSnapshot(ctx, store.FrozenFailRunsMissingFromSnapshotParams{
						WorkerID: pgconv.UUID(f.worker), MissingCutoff: cutoff, MaxRequeues: 1,
						Now: observed, GlobalTimeoutSeconds: 3600, FrozenTargets: ledger, LockedParentIds: ids,
						FailureReason: pgconv.TextOrNull("iteration recovery control"),
					})
					if err != nil || len(rows) != 1 || rows[0].ID != control {
						t.Fatalf("frozen missing exhaustion=%v err=%v; want only control", rows, err)
					}
				} else {
					rows, err := q.FailRunsMissingFromSnapshot(ctx, store.FailRunsMissingFromSnapshotParams{
						WorkerID: pgconv.UUID(f.worker), MissingCutoff: cutoff, MaxRequeues: 1,
						Now: observed, GlobalTimeoutSeconds: 3600,
						FailureReason: pgconv.TextOrNull("iteration recovery control"),
					})
					if err != nil || len(rows) != 1 || rows[0].ID != control {
						t.Fatalf("missing exhaustion=%v err=%v; want only control", rows, err)
					}
				}
			})
		})
	}
}

func TestWorkerIterationParkSnapshotsLiveDB(t *testing.T) {
	f := iterationRoutes(t)
	ctx := context.Background()
	id := f.run(t, "prompt")
	body := strings.Replace(iterationParkBody, `"head":""`, `"head":"snapshot-head"`, 1)
	assertIterationAck(t, f.call(id, "iteration-park", body, f.worker), 200, "")
	f.exec(t, "UPDATE runs SET status_since=now()-interval '10 minutes',requeue_count=1 WHERE id=$1", id)
	f.exec(t, "UPDATE workers SET snapshot_register_nonce='iteration-snapshot',snapshot_epoch=0 WHERE id=$1", f.worker)
	before := f.snapshot(t, id)
	svc := workersvc.New(f.q, newHandlerTestBox(t), workersvc.Params{
		RunMaxIterations: 5, RunTimeout: time.Hour, RunMaxRequeues: 1,
		WorkerHeartbeatStale: time.Minute, WorkerHeartbeatInterval: time.Second,
		ActiveSnapshotMaxEntries: 8, WorkerOutboxMaxPending: 8,
	})
	svc.SetTxBeginner(f.pool)
	for _, step := range []struct {
		name     string
		register bool
		listed   bool
		epoch    int64
	}{
		{"heartbeat_listed", false, true, 1},
		{"heartbeat_missing", false, false, 2},
		{"register_listed", true, true, 0},
		{"register_missing", true, false, 0},
	} {
		t.Run(step.name, func(t *testing.T) {
			wkr, err := f.q.GetWorkerByID(ctx, f.worker)
			if err != nil {
				t.Fatal(err)
			}
			snap := &workersvc.ActiveSnapshot{SnapshotEpoch: step.epoch, Active: []workersvc.ActiveRunEntry{}}
			if step.listed {
				snap.Active = append(snap.Active, workersvc.ActiveRunEntry{
					RunID: id.String(), ClaimGeneration: 4, Phase: "running",
				})
			}
			if step.register {
				_, nonce, err := svc.Register(ctx, wkr, "v1", "base", nil, nil, nil, snap)
				if err != nil || nonce == "" || nonce == wkr.SnapshotRegisterNonce.String {
					t.Fatalf("register nonce=%q err=%v; want a fresh nonce", nonce, err)
				}
				stored, err := f.q.GetWorkerByID(ctx, f.worker)
				if err != nil || stored.SnapshotRegisterNonce.String != nonce || stored.SnapshotEpoch != 0 {
					t.Fatalf("register did not persist the rotated nonce and reset epoch: %v", err)
				}
			} else {
				snap.RegisterNonce = wkr.SnapshotRegisterNonce.String
				if _, err := svc.Heartbeat(ctx, wkr, nil, nil, snap); err != nil {
					t.Fatal(err)
				}
				stored, err := f.q.GetWorkerByID(ctx, f.worker)
				if err != nil || stored.SnapshotEpoch != step.epoch {
					t.Fatalf("heartbeat did not accept snapshot epoch %d: %v", step.epoch, err)
				}
			}
			var count int
			var exact bool
			err = f.pool.QueryRow(ctx, `SELECT count(*),COALESCE(bool_and(
 claim_generation=4 AND phase='running' AND NOT terminal_pending AND snapshot_epoch=$3),false)
 FROM worker_active_runs WHERE worker_id=$1 AND run_id=$2`, f.worker, id, step.epoch).Scan(&count, &exact)
			want := 0
			if step.listed {
				want = 1
			}
			if err != nil || count != want || (step.listed && !exact) {
				t.Fatalf("snapshot storage count=%d exact=%v want=%d err=%v", count, exact, want, err)
			}
			if f.snapshot(t, id) != before {
				t.Fatal("snapshot recovery changed the iteration hold, generation, captured head or budgets")
			}
		})
	}
}

func TestWorkerIterationParkLateWritesLiveDB(t *testing.T) {
	f := iterationRoutes(t)
	ctx := context.Background()
	id := f.run(t, "prompt")
	assertIterationAck(t, f.call(id, "iteration-park", iterationParkBody, f.worker), 200, "")
	before := f.snapshot(t, id)
	// Legacy and generation-stamped state reports both retain the iteration hold.
	for _, gen := range []string{"", `,"claim_generation":4`} {
		for _, state := range []string{"running", "awaiting_approval", "awaiting_input", "awaiting_followup", "failed", "completed", "paused", "limit_wait", "recovery_wait"} {
			rec := f.call(id, "state", fmt.Sprintf(`{"status":%q%s}`, state, gen), f.worker)
			if rec.Code == 200 {
				t.Fatalf("late state %s accepted: %s", state, rec.Body.String())
			}
			if f.snapshot(t, id) != before {
				t.Fatalf("late %s changed held state", state)
			}
		}
	}
	// Exercise SQL guards directly as well as the HTTP service fence.
	worker := pgconv.UUID(f.worker)
	if n, err := f.q.SetRunAwaitingInput(ctx, store.SetRunAwaitingInputParams{ID: id, WorkerID: worker}); err != nil || n != 0 {
		t.Fatalf("input=%d %v", n, err)
	}
	if n, err := f.q.SetRunAwaitingFollowup(ctx, store.SetRunAwaitingFollowupParams{ID: id, WorkerID: worker}); err != nil || n != 0 {
		t.Fatalf("followup=%d %v", n, err)
	}
	if n, err := f.q.SetRunCompleted(ctx, store.SetRunCompletedParams{ID: id, WorkerID: worker}); err != nil || n != 0 {
		t.Fatalf("completed=%d %v", n, err)
	}
	if n, err := f.q.SetRunFailed(ctx, store.SetRunFailedParams{ID: id, WorkerID: worker}); err != nil || n != 0 {
		t.Fatalf("failed=%d %v", n, err)
	}
	if n, err := f.q.SetRunFailedPlanRejected(ctx, store.SetRunFailedPlanRejectedParams{ID: id, WorkerID: worker}); err != nil || n != 0 {
		t.Fatalf("rejected=%d %v", n, err)
	}
	if n, err := f.q.CancelRunByWorker(ctx, store.CancelRunByWorkerParams{ID: id, WorkerID: worker}); err != nil || n != 0 {
		t.Fatalf("cancel=%d %v", n, err)
	}
	if n, err := f.q.SupersedeRunByWorker(ctx, store.SupersedeRunByWorkerParams{ID: id, WorkerID: worker}); err != nil || n != 0 {
		t.Fatalf("supersede=%d %v", n, err)
	}
	if f.snapshot(t, id) != before {
		t.Fatal("late SQL writer changed hold")
	}
	f.exec(t, "UPDATE runs SET completion_contract_version=1,contract_revision=1 WHERE id=$1", id)
	before = f.snapshot(t, id)
	_, err := f.q.RecordCompletionAttempt(ctx, store.RecordCompletionAttemptParams{RunID: id, WorkerID: worker, Unmet: []byte("[]")})
	if err != pgx.ErrNoRows {
		t.Fatalf("late attempt=%v", err)
	}
	if f.snapshot(t, id) != before {
		t.Fatal("late attempt inserted evidence")
	}
	// Explicit owner cancellation remains functional.
	if n, err := f.q.CancelRunServerSide(ctx, store.CancelRunServerSideParams{ID: id, UserID: f.user}); err != nil || n != 1 {
		t.Fatalf("owner cancel=%d %v", n, err)
	}
	r, err := f.q.GetRunByID(ctx, id)
	if err != nil || r.Status != "cancelled" {
		t.Fatalf("owner cancel status=%s %v", r.Status, err)
	}
}
