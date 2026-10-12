package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func stringPtrForPause(s string) *string { return &s }

type pauseTestBeginner struct {
	pool           *pgxpool.Pool
	beforeLock     func()
	beforeCommit   func() error
	failSettlement bool
}

func (b pauseTestBeginner) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	if b.beforeLock != nil {
		b.beforeLock()
	}
	return &pauseTestTx{Tx: tx, beforeCommit: b.beforeCommit, failSettlement: b.failSettlement}, nil
}

type pauseTestTx struct {
	pgx.Tx
	beforeCommit   func() error
	failSettlement bool
}

func (tx *pauseTestTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if tx.failSettlement && strings.Contains(sql, "-- name: ConsumeWorkerPauseInputs") {
		return pgconn.CommandTag{}, errors.New("injected pause settlement failure")
	}
	return tx.Tx.Exec(ctx, sql, args...)
}

func (tx *pauseTestTx) Commit(ctx context.Context) error {
	if tx.beforeCommit != nil {
		if err := tx.beforeCommit(); err != nil {
			return err
		}
	}
	return tx.Tx.Commit(ctx)
}

func TestWorkerPauseLiveDB(t *testing.T) {
	e := setupJobRouteLiveDB(t)
	ctx := context.Background()
	conn, repo := uuid.New(), uuid.New()
	// Parent cleanup follows all subtests and precedes setupJobRouteLiveDB's pool
	// close. Active fixtures must not remain visible to later retention sweeps.
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// This fresh owner has only this fixture's rows; the existing foreign keys
		// cascade to its runs (and inputs/messages), both workers, connection and repo.
		if _, err := e.pool.Exec(cleanupCtx, "DELETE FROM users WHERE id=$1", e.user); err != nil {
			t.Errorf("delete pause fixture owner: %v", err)
		}
		var remaining bool
		if err := e.pool.QueryRow(cleanupCtx, `SELECT
 EXISTS (SELECT 1 FROM users WHERE id=$1) OR
 EXISTS (SELECT 1 FROM runs WHERE user_id=$1 OR repo_id=$3) OR
 EXISTS (SELECT 1 FROM workers WHERE user_id=$1) OR
 EXISTS (SELECT 1 FROM forge_connections WHERE id=$2 OR user_id=$1) OR
 EXISTS (SELECT 1 FROM repos WHERE id=$3 OR connection_id=$2)`, e.user, conn, repo).Scan(&remaining); err != nil {
			t.Errorf("check pause fixture cleanup: %v", err)
		} else if remaining {
			t.Error("pause fixture cleanup left owned rows in the database")
		}
	})
	e.exec(t, `INSERT INTO forge_connections (id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext)
 VALUES ($1,$2,'gitlab','https://forge.e2e','bot',1,$3)`, conn, e.user, []byte{1})
	e.exec(t, `INSERT INTO repos (id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch,enabled)
 VALUES ($1,$2,1,$3,'https://forge.e2e/g/r','main',true)`, repo, conn, "park-"+repo.String())
	e.exec(t, `UPDATE workers SET protocol_capabilities=$2 WHERE id=$1`, e.worker, []string{capability.ParkRunV1})
	q := store.New(e.pool)
	newRun := func(t *testing.T) uuid.UUID {
		t.Helper()
		id := uuid.New()
		e.exec(t, `INSERT INTO runs (id,user_id,repo_id,issue_iid,issue_title,issue_description,status,worker_id,claim_generation,plan_md,auto_approve,started_at,budget_wall_seconds)
 VALUES ($1,$2,$3,$4,'t','d','running',$5,4,'approved plan',true,now(),3600)`, id, e.user, repo, int64(id[0])+int64(id[1])*256+int64(id[2])*65536, e.worker)
		return id
	}
	call := func(t *testing.T, id uuid.UUID, endpoint, body string, worker uuid.UUID) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/worker/runs/"+id.String()+"/"+endpoint, strings.NewReader(body))
		if worker != uuid.Nil {
			req.Header.Set("Authorization", "Bearer "+e.tokens[worker])
		}
		rec := httptest.NewRecorder()
		e.router.ServeHTTP(rec, req)
		return rec
	}
	assertPause := func(t *testing.T, rec *httptest.ResponseRecorder, code int, applied bool, status string) {
		t.Helper()
		var got struct {
			Applied bool `json:"applied"`
			Run     struct {
				Status string `json:"status"`
			} `json:"run"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if rec.Code != code || got.Applied != applied || got.Run.Status != status {
			t.Fatalf("pause = %d %s; want %d applied=%v status=%s", rec.Code, rec.Body.String(), code, applied, status)
		}
	}
	// These expiry fixtures model an attended run with a seeded approved plan.
	// Keep the ordinary fixtures auto-approved for their existing refusal cases.
	newAttendedRun := func(t *testing.T, started time.Time) uuid.UUID {
		t.Helper()
		id := newRun(t)
		e.exec(t, "UPDATE runs SET auto_approve=false,plan_source='seeded',started_at=$2 WHERE id=$1", id, started)
		return id
	}
	snapshot := func(t *testing.T, id uuid.UUID) string {
		t.Helper()
		var value string
		if err := e.pool.QueryRow(ctx, `SELECT jsonb_build_object(
 'run',to_jsonb(r), 'messages',(SELECT jsonb_agg(m ORDER BY seq) FROM run_messages m WHERE run_id=r.id),
 'inputs',(SELECT jsonb_agg(i ORDER BY id) FROM run_user_inputs i WHERE run_id=r.id))::text FROM runs r WHERE id=$1`, id).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	useClock := func(t *testing.T, clock *time.Time, beforeLock func()) {
		t.Helper()
		svc := workersvc.New(q, newHandlerTestBox(t), workersvc.Params{})
		svc.SetClock(func() time.Time { return *clock })
		svc.SetTxBeginner(pauseTestBeginner{pool: e.pool, beforeLock: beforeLock})
		h := &Handler{q: q, wsvc: svc, now: func() time.Time { return *clock }}
		previous := e.router
		e.router = h.WorkerRoutes(mw.NewLimiter(1000, time.Minute, nil))
		t.Cleanup(func() { e.router = previous })
	}
	assertUnchanged := func(t *testing.T, id uuid.UUID, before string) {
		t.Helper()
		if snapshot(t, id) != before {
			t.Fatal("refusal changed run, question, accounting, messages or inputs")
		}
		run, err := q.GetRunByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if run.Status != "running" || run.OpenQuestionID.Valid || run.PauseRequestedAt.Valid ||
			run.PauseMode.Valid || run.PauseAfterCount.Valid {
			t.Fatalf("refusal persisted question or pause: %+v", run)
		}
		var count int
		if err := e.pool.QueryRow(ctx, "SELECT count(*) FROM run_user_inputs WHERE run_id=$1 AND kind='pause'", id).Scan(&count); err != nil || count != 0 {
			t.Fatalf("pause inputs=%d err=%v", count, err)
		}
	}
	assertExpired := func(t *testing.T, rec *httptest.ResponseRecorder) {
		t.Helper()
		assertPause(t, rec, http.StatusConflict, false, "running")
		var ack struct {
			Applied *bool  `json:"applied"`
			Reason  string `json:"reason"`
			Run     struct {
				ID uuid.UUID `json:"id"`
			} `json:"run"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &ack); err != nil {
			t.Fatal(err)
		}
		if ack.Applied == nil || *ack.Applied || ack.Reason != "wall_expired" || ack.Run.ID == uuid.Nil {
			t.Fatalf("expiry ACK=%s", rec.Body.String())
		}
	}
	const guardedQuestion = `{"status":"awaiting_input","claim_generation":4,"require_running":true,"open_question_id":"expired-question"}`
	started := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	for _, elapsed := range []time.Duration{time.Hour, time.Hour + time.Second} {
		for _, endpoint := range []string{"pause", "state"} {
			t.Run(fmt.Sprintf("expiry_before_%s_%s", endpoint, elapsed), func(t *testing.T) {
				id := newAttendedRun(t, started)
				clock := started.Add(elapsed)
				useClock(t, &clock, nil)
				before := snapshot(t, id)
				body := guardedQuestion
				if endpoint == "pause" {
					body = `{"claim_generation":4}`
				}
				assertExpired(t, call(t, id, endpoint, body, e.worker))
				assertUnchanged(t, id, before)
			})
		}
	}
	t.Run("expiry_between_scope_pause_refusal_and_question", func(t *testing.T) {
		id := newAttendedRun(t, started)
		e.exec(t, `UPDATE runs SET scope_ceiling=1,milestones_completed='["m1"]',
 milestones_frozen='[{"id":"m1","title":"one"},{"id":"m2","title":"two"}]' WHERE id=$1`, id)
		clock := started.Add(time.Hour - time.Second)
		useClock(t, &clock, nil)
		before := snapshot(t, id)
		rec := call(t, id, "pause", `{"claim_generation":4}`, e.worker)
		assertPause(t, rec, http.StatusConflict, false, "running")
		if strings.Contains(rec.Body.String(), "wall_expired") {
			t.Fatal("scope refusal classified as expiry before deadline")
		}
		assertUnchanged(t, id, before)
		clock = started.Add(time.Hour)
		assertExpired(t, call(t, id, "state", guardedQuestion, e.worker))
		assertUnchanged(t, id, before)
	})
	t.Run("expiry_after_unlocked_snapshot", func(t *testing.T) {
		id := newAttendedRun(t, started)
		clock := started.Add(time.Hour - time.Second)
		useClock(t, &clock, func() {
			// Begin follows the unlocked ownership snapshot. Change the persisted
			// start so only the locked row has an elapsed wall.
			e.exec(t, "UPDATE runs SET started_at=$2 WHERE id=$1", id, started.Add(-time.Second))
		})
		e.exec(t, "UPDATE runs SET started_at=$2 WHERE id=$1", id, started.Add(-time.Second))
		expiredSnapshot := snapshot(t, id)
		e.exec(t, "UPDATE runs SET started_at=$2 WHERE id=$1", id, started)
		assertExpired(t, call(t, id, "state", guardedQuestion, e.worker))
		assertUnchanged(t, id, expiredSnapshot)
	})
	for _, change := range []string{"claim_generation=5", "claim_released_at=now()"} {
		for _, endpoint := range []string{"pause", "state"} {
			t.Run("expired_"+endpoint+"_"+change, func(t *testing.T) {
				id := newAttendedRun(t, started)
				e.exec(t, "UPDATE runs SET "+change+" WHERE id=$1", id)
				clock := started.Add(2 * time.Hour)
				useClock(t, &clock, nil)
				before := snapshot(t, id)
				body := guardedQuestion
				if endpoint == "pause" {
					body = `{"claim_generation":4}`
				}
				rec := call(t, id, endpoint, body, e.worker)
				if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"disposition":"stale_claim"`) ||
					strings.Contains(rec.Body.String(), "wall_expired") {
					t.Fatalf("stale precedence=%d %s", rec.Code, rec.Body.String())
				}
				assertUnchanged(t, id, before)
			})
		}
	}
	t.Run("accepted_question_replay_after_elapsed_wall", func(t *testing.T) {
		id := newAttendedRun(t, started)
		clock := started.Add(time.Minute)
		useClock(t, &clock, nil)
		assertPause(t, call(t, id, "state", guardedQuestion, e.worker), http.StatusOK, true, "awaiting_input")
		run, err := q.GetRunByID(ctx, id)
		if err != nil || run.OpenQuestionID.String != "expired-question" || run.BudgetWallSeconds.Int32 != 3600 ||
			run.AutoApprove || run.PauseRequestedAt.Valid {
			t.Fatalf("accepted question: run=%+v err=%v", run, err)
		}
		before := snapshot(t, id)
		clock = started.Add(2 * time.Hour)
		assertPause(t, call(t, id, "state", guardedQuestion, e.worker), http.StatusOK, true, "awaiting_input")
		if snapshot(t, id) != before {
			t.Fatal("elapsed accepted replay changed question, accounting, messages or inputs")
		}
	})
	t.Run("request_duplicate_ACK_delayed_question_owner_resume", func(t *testing.T) {
		id := newRun(t)
		assertPause(t, call(t, id, "pause", `{"claim_generation":4}`, e.worker), 200, true, "running")
		assertPause(t, call(t, id, "pause", `{"claim_generation":4}`, e.worker), 200, true, "running")
		run, err := q.GetRunByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if !run.PauseRequestedAt.Valid || run.PauseMode.String != "now" || !run.PauseAfterCount.Valid {
			t.Fatalf("pending pause: %+v", run)
		}
		var total, settled int
		if err := e.pool.QueryRow(ctx, `SELECT count(*),count(*) FILTER (WHERE consumed_at IS NOT NULL AND applied_at IS NOT NULL AND consumed_claim_generation=4 AND consumed_worker_id=$2) FROM run_user_inputs WHERE run_id=$1 AND kind='pause'`, id, e.worker).Scan(&total, &settled); err != nil {
			t.Fatal(err)
		}
		if total != 1 || settled != 1 {
			t.Fatalf("pause audit total=%d settled=%d", total, settled)
		}
		inputs, err := q.ConsumeRunInputs(ctx, id)
		if err != nil || len(inputs) != 0 {
			t.Fatalf("steering=%v err=%v", inputs, err)
		}
		ack := call(t, id, "state", `{"status":"paused","claim_generation":4}`, e.worker)
		if ack.Code != 200 {
			t.Fatalf("ACK=%d %s", ack.Code, ack.Body.String())
		}
		delayed := call(t, id, "state", `{"status":"awaiting_input","claim_generation":4,"require_running":true,"open_question_id":"late"}`, e.worker)
		if delayed.Code != 409 || !strings.Contains(delayed.Body.String(), `"status":"paused"`) {
			t.Fatalf("delayed=%d %s", delayed.Code, delayed.Body.String())
		}
		run, err = q.GetRunByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if run.Status != "paused" || run.OpenQuestionID.Valid || run.PauseRequestedAt.Valid {
			t.Fatalf("delayed write changed pause: status=%s question=%v", run.Status, run.OpenQuestionID)
		}
		// Owner authorization and ordinary resume remain the existing public handler.
		h := &Handler{q: q}
		route := chi.NewRouter()
		route.Post("/api/runs/{id}/resume-now", h.ResumeRunNow)
		req := httptest.NewRequest(http.MethodPost, "/api/runs/"+id.String()+"/resume-now", nil)
		req = req.WithContext(mw.ContextWithUser(req.Context(), store.User{ID: e.user}))
		rec := httptest.NewRecorder()
		route.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("resume=%d %s", rec.Code, rec.Body.String())
		}
		run, err = q.GetRunByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if run.Status != "queued" || run.PauseRequestedAt.Valid {
			t.Fatalf("resume status=%s pending=%v", run.Status, run.PauseRequestedAt)
		}
	})
	t.Run("ordinary_pause_preserves_wall_allowance_and_resume_banks_once", func(t *testing.T) {
		for _, pausedSeconds := range []int32{120, 7200} {
			t.Run(fmt.Sprintf("paused_%ds", pausedSeconds), func(t *testing.T) {
				id := newRun(t)
				const tip = "0123456789abcdef0123456789abcdef01234567"
				e.exec(t, `UPDATE runs SET budget_paused_seconds=60,session_id='kept-session',
 branch='agent/park-budget',checkpoint_tip=$2,checkpoint_tip_at=now() WHERE id=$1`, id, tip)
				before, err := q.GetRunByID(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				assertPause(t, call(t, id, "pause", `{"claim_generation":4}`, e.worker), 200, true, "running")
				ack := call(t, id, "state", `{"status":"paused","claim_generation":4}`, e.worker)
				if ack.Code != http.StatusOK {
					t.Fatalf("pause ACK=%d %s", ack.Code, ack.Body.String())
				}
				assertKept := func(run store.Run) {
					t.Helper()
					if run.WorkerID != before.WorkerID || run.SessionID != before.SessionID ||
						run.Branch != before.Branch || run.CheckpointTip != before.CheckpointTip ||
						run.CheckpointTipAt != before.CheckpointTipAt || run.ClaimGeneration != 4 ||
						run.ClaimReleasedAt.Valid || run.HoldReason.Valid || run.FinishedAt.Valid {
						t.Fatal("ordinary pause/resume changed affinity, session, checkpoint, claim or hold")
					}
				}
				parked, err := q.GetRunByID(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				assertKept(parked)
				if parked.Status != "paused" || parked.BudgetPausedSeconds != 60 ||
					parked.StartedAt != before.StartedAt || parked.PauseRequestedAt.Valid ||
					parked.PauseMode.Valid || parked.PauseAfterCount.Valid {
					t.Fatal("pause ACK changed accounting or left pending pause")
				}

				// PostgreSQL now() is fixed within this transaction: ResumePausedRun
				// sees exactly pausedSeconds, without sleeping or rounding tolerance.
				tx, err := e.pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = tx.Rollback(ctx) }()
				var resumeAt time.Time
				if err := tx.QueryRow(ctx, "SELECT now()").Scan(&resumeAt); err != nil {
					t.Fatal(err)
				}
				parkAt := resumeAt.Add(-time.Duration(pausedSeconds) * time.Second)
				startedAt := parkAt.Add(-600 * time.Second)
				if _, err := tx.Exec(ctx, `UPDATE runs SET started_at=$2,status_since=$3 WHERE id=$1`, id, startedAt, parkAt); err != nil {
					t.Fatal(err)
				}
				tq := store.New(tx)
				clock := parkAt
				svc := workersvc.New(tq, newHandlerTestBox(t), workersvc.Params{})
				svc.SetTxBeginner(e.pool)
				h := &Handler{q: tq, wsvc: svc,
					now: func() time.Time { return clock }}
				route := chi.NewRouter()
				route.Get("/api/runs/{id}", h.GetRun)
				route.Post("/api/runs/{id}/resume-now", h.ResumeRunNow)
				ownerCall := func(method, suffix string) *httptest.ResponseRecorder {
					t.Helper()
					req := httptest.NewRequest(method, "/api/runs/"+id.String()+suffix, nil)
					req = req.WithContext(mw.ContextWithUser(req.Context(), store.User{ID: e.user}))
					rec := httptest.NewRecorder()
					route.ServeHTTP(rec, req)
					return rec
				}
				assertBudget := func(rec *httptest.ResponseRecorder, status string) {
					t.Helper()
					var body struct {
						Run apitypes.RunDTO `json:"run"`
					}
					if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
						t.Fatal(err)
					}
					if rec.Code != http.StatusOK || body.Run.Status != status ||
						body.Run.BudgetTotalSeconds == nil || *body.Run.BudgetTotalSeconds != 3600 ||
						body.Run.BudgetUsedSeconds == nil || *body.Run.BudgetUsedSeconds != 540 ||
						body.Run.DeadlineAt != nil {
						t.Fatalf("budget at %s = %d %s; want %s total=3600 used=540 remaining=3060 no deadline",
							clock, rec.Code, rec.Body.String(), status)
					}
				}
				assertBudget(ownerCall(http.MethodGet, ""), "paused")
				clock = resumeAt
				assertBudget(ownerCall(http.MethodGet, ""), "paused")
				clock = resumeAt.Add(24 * time.Hour)
				assertBudget(ownerCall(http.MethodGet, ""), "paused")
				clock = resumeAt
				assertBudget(ownerCall(http.MethodPost, "/resume-now"), "queued")
				resumed, err := tq.GetRunByID(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				assertKept(resumed)
				if resumed.BudgetPausedSeconds != 60+pausedSeconds ||
					!resumed.StartedAt.Time.Equal(startedAt) || !resumed.StatusSince.Time.Equal(resumeAt) ||
					resumed.BudgetWallSeconds != before.BudgetWallSeconds ||
					resumed.BudgetExtensionSeconds != 0 || resumed.BudgetFinalizeSeconds != 0 {
					t.Fatalf("resume accounting: banked=%d started=%v since=%v", resumed.BudgetPausedSeconds, resumed.StartedAt, resumed.StatusSince)
				}
				snapshot := func() string {
					t.Helper()
					var value string
					if err := tx.QueryRow(ctx, `SELECT to_jsonb(r)::text FROM runs r WHERE id=$1`, id).Scan(&value); err != nil {
						t.Fatal(err)
					}
					return value
				}
				firstResume := snapshot()
				if rec := ownerCall(http.MethodPost, "/resume-now"); rec.Code != http.StatusConflict {
					t.Fatalf("duplicate resume=%d %s", rec.Code, rec.Body.String())
				}
				if snapshot() != firstResume {
					t.Fatal("duplicate resume changed the run or banked the pause twice")
				}
				assertBudget(ownerCall(http.MethodGet, ""), "queued")
				var audits int
				if err := tx.QueryRow(ctx, `SELECT count(*) FROM run_user_inputs WHERE run_id=$1 AND kind='resume'`, id).Scan(&audits); err != nil {
					t.Fatal(err)
				}
				if audits != 1 {
					t.Fatalf("resume audits=%d; want exactly one", audits)
				}
				if err := tx.Commit(ctx); err != nil {
					t.Fatal(err)
				}
			})
		}
	})
	cases := []struct{ name, sql string }{
		{"unsupported", "UPDATE workers SET protocol_capabilities='{}' WHERE id=$2"},
		{"wrong_generation", "UPDATE runs SET claim_generation=5 WHERE id=$1"},
		{"released_generation", "UPDATE runs SET claim_released_at=now() WHERE id=$1"},
		{"credential_switch", "UPDATE runs SET credential_switch_requested_at=now(),credential_switch_generation=4 WHERE id=$1"},
		{"paused", "UPDATE runs SET status='paused' WHERE id=$1"},
		{"claimed", "UPDATE runs SET status='claimed' WHERE id=$1"},
		{"awaiting_approval", "UPDATE runs SET status='awaiting_approval' WHERE id=$1"},
		{"awaiting_input", "UPDATE runs SET status='awaiting_input' WHERE id=$1"},
		{"finalizing", "UPDATE runs SET budget_finalize_seconds=60 WHERE id=$1"},
		{"interactive", "UPDATE runs SET interactive=true WHERE id=$1"},
		{"nonissue", "UPDATE runs SET kind='prompt',issue_iid=NULL WHERE id=$1"},
		{"no_plan", "UPDATE runs SET plan_md=NULL WHERE id=$1"},
		{"blank_plan", "UPDATE runs SET plan_md='   ' WHERE id=$1"},
		{"unapproved", "UPDATE runs SET auto_approve=false WHERE id=$1"},
		{"stop", "UPDATE runs SET stop_kind='stopped' WHERE id=$1"},
		{"scope_stop", "UPDATE runs SET scope_ceiling=0 WHERE id=$1"},
		{"wall_request", "UPDATE runs SET pause_requested_at=now(),pause_mode='wall' WHERE id=$1"},
		{"wall_expired", "UPDATE runs SET started_at=now()-interval '2 hours' WHERE id=$1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := newRun(t)
			// Supply only the parameters the individual SQL statement uses.
			if tc.name == "unsupported" {
				e.exec(t, strings.ReplaceAll(tc.sql, "$2", "$1"), e.worker)
			} else {
				e.exec(t, tc.sql, id)
			}
			defer e.exec(t, `UPDATE workers SET protocol_capabilities=$2 WHERE id=$1`, e.worker, []string{capability.ParkRunV1})
			before, err := q.GetRunByID(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			rec := call(t, id, "pause", `{"claim_generation":4}`, e.worker)
			assertPause(t, rec, 409, false, before.Status)
			if (tc.name == "wrong_generation" || tc.name == "released_generation") && !strings.Contains(rec.Body.String(), `"disposition":"stale_claim"`) {
				t.Fatalf("stale disposition missing: %s", rec.Body.String())
			}
			if tc.name == "wall_request" && !strings.Contains(rec.Body.String(), `"reason":"wall_pending"`) {
				t.Fatalf("wall disposition missing: %s", rec.Body.String())
			}
			if tc.name == "credential_switch" && !strings.Contains(rec.Body.String(), `"credential_switch":{"generation":4}`) {
				t.Fatalf("switch disposition missing: %s", rec.Body.String())
			}
			var n int
			if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM run_user_inputs WHERE run_id=$1`, id).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != 0 {
				t.Fatalf("refused pause wrote %d inputs", n)
			}
		})
	}
	t.Run("worker_query_fences", func(t *testing.T) {
		cases := []string{
			"claim_generation=5", "worker_id=NULL", "claim_released_at=now()",
			"status='paused'", "kind='prompt',issue_iid=NULL", "interactive=true",
			"pause_requested_at=now(),pause_mode='now'",
			"pause_mode='wall'", "stop_kind='stopped'", "budget_finalize_seconds=60",
		}
		for _, change := range cases {
			id := newRun(t)
			e.exec(t, "UPDATE runs SET "+change+" WHERE id=$1", id)
			_, err := q.CreateWorkerPauseInput(ctx, store.CreateWorkerPauseInputParams{
				ID: id, WorkerID: pgconv.UUID(e.worker), ClaimGeneration: 4,
			})
			if !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("%s: query err=%v", change, err)
			}
			var count int
			if err := e.pool.QueryRow(ctx, "SELECT count(*) FROM run_user_inputs WHERE run_id=$1", id).Scan(&count); err != nil || count != 0 {
				t.Fatalf("%s: inputs=%d err=%v", change, count, err)
			}
		}
	})
	t.Run("pause_settlement_failure_rolls_back", func(t *testing.T) {
		id := newRun(t)
		svc := workersvc.New(q, newHandlerTestBox(t), workersvc.Params{})
		svc.SetTxBeginner(pauseTestBeginner{pool: e.pool, failSettlement: true})
		worker, err := q.GetWorkerByID(ctx, e.worker)
		if err != nil {
			t.Fatal(err)
		}
		_, applied, err := svc.RequestWorkerPause(ctx, worker, id, 4)
		if err == nil || applied {
			t.Fatalf("failed settlement: applied=%v err=%v", applied, err)
		}
		run, err := q.GetRunByID(ctx, id)
		if err != nil || run.PauseRequestedAt.Valid || run.PauseMode.Valid || run.PauseAfterCount.Valid {
			t.Fatalf("rollback left pause fields: pending=%v mode=%v floor=%v err=%v", run.PauseRequestedAt, run.PauseMode, run.PauseAfterCount, err)
		}
		var count int
		if err := e.pool.QueryRow(ctx, "SELECT count(*) FROM run_user_inputs WHERE run_id=$1", id).Scan(&count); err != nil || count != 0 {
			t.Fatalf("rollback inputs=%d err=%v", count, err)
		}
	})
	t.Run("require_running_reads_locked_row_and_holds_lock", func(t *testing.T) {
		worker, err := q.GetWorkerByID(ctx, e.worker)
		if err != nil {
			t.Fatal(err)
		}
		gen := int64(4)
		for _, change := range []string{"status='paused'", "claim_generation=5", "claim_released_at=now()"} {
			id := newRun(t)
			svc := workersvc.New(q, newHandlerTestBox(t), workersvc.Params{})
			svc.SetTxBeginner(pauseTestBeginner{pool: e.pool, beforeLock: func() {
				e.exec(t, "UPDATE runs SET "+change+" WHERE id=$1", id)
			}})
			run, applied, err := svc.SetState(ctx, worker, id, workersvc.StateRequest{
				State: "awaiting_input", ClaimGeneration: &gen, RequireRunning: true,
				OpenQuestionID: stringPtrForPause("late"),
			})
			if applied || run.OpenQuestionID.Valid || errors.Is(err, workersvc.ErrStaleClaim) != (change != "status='paused'") {
				t.Fatalf("%s: applied=%v question=%v err=%v", change, applied, run.OpenQuestionID, err)
			}
			if change == "status='paused'" && err != nil {
				t.Fatal(err)
			}
		}
		id := newRun(t)
		svc := workersvc.New(q, newHandlerTestBox(t), workersvc.Params{})
		checked := false
		svc.SetTxBeginner(pauseTestBeginner{pool: e.pool, beforeCommit: func() error {
			checked = true
			_, err := e.pool.Exec(ctx, "SELECT id FROM runs WHERE id=$1 FOR UPDATE NOWAIT", id)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
				return fmt.Errorf("run lock not held through question commit: %v", err)
			}
			return nil
		}})
		run, applied, err := svc.SetState(ctx, worker, id, workersvc.StateRequest{
			State: "awaiting_input", ClaimGeneration: &gen, RequireRunning: true,
			OpenQuestionID: stringPtrForPause("normal"),
		})
		if err != nil || !applied || !checked || run.Status != "awaiting_input" || run.OpenQuestionID.String != "normal" {
			t.Fatalf("live question: applied=%v checked=%v status=%s question=%v err=%v", applied, checked, run.Status, run.OpenQuestionID, err)
		}
	})
	t.Run("authentication_and_generation_validation", func(t *testing.T) {
		id := newRun(t)
		if rec := call(t, id, "pause", `{"claim_generation":4}`, uuid.Nil); rec.Code != 401 {
			t.Fatalf("unauth=%d", rec.Code)
		}
		other := e.seedWorker(t, e.user)
		if rec := call(t, id, "pause", `{"claim_generation":4}`, other); rec.Code != 404 {
			t.Fatalf("other worker=%d %s", rec.Code, rec.Body.String())
		}
		for _, body := range []string{`{}`, `{"claim_generation":0}`, `{"claim_generation":-1}`, `{"claim_generation":4,"mode":"milestone"}`} {
			if rec := call(t, id, "pause", body, e.worker); rec.Code != 400 {
				t.Fatalf("body=%s response=%d %s", body, rec.Code, rec.Body.String())
			}
		}
	})
	t.Run("existing_owner_pause_no_duplicate", func(t *testing.T) {
		id := newRun(t)
		if _, err := q.CreatePauseInput(ctx, store.CreatePauseInputParams{ID: id, Mode: pgconv.TextOrNull("milestone")}); err != nil {
			t.Fatal(err)
		}
		assertPause(t, call(t, id, "pause", `{"claim_generation":4}`, e.worker), 200, true, "running")
		run, err := q.GetRunByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if run.PauseMode.String != "milestone" {
			t.Fatalf("mode=%s", run.PauseMode.String)
		}
		var n int
		if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM run_user_inputs WHERE run_id=$1 AND kind='pause'`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("duplicate audit inputs=%d", n)
		}
		replay, err := q.ListReplayRunInputs(ctx, id)
		if err != nil || len(replay) != 0 {
			t.Fatalf("replay=%v err=%v", replay, err)
		}
	})
	t.Run("pending_stop_and_unconsumed_approval", func(t *testing.T) {
		for _, kind := range []string{"stop", "cancel", "approve_plan"} {
			id := newRun(t)
			if kind == "approve_plan" {
				e.exec(t, "UPDATE runs SET auto_approve=false WHERE id=$1", id)
			}
			e.exec(t, "INSERT INTO run_user_inputs(run_id,kind,body) VALUES($1,$2,'')", id, kind)
			assertPause(t, call(t, id, "pause", `{"claim_generation":4}`, e.worker), 409, false, "running")
			run, err := q.GetRunByID(ctx, id)
			if err != nil || run.PauseRequestedAt.Valid {
				t.Fatalf("refusal changed pause: pending=%v err=%v", run.PauseRequestedAt, err)
			}
		}
	})
	t.Run("human_and_seeded_approval", func(t *testing.T) {
		for _, source := range []string{"human", "seeded"} {
			id := newRun(t)
			e.exec(t, `UPDATE runs SET auto_approve=false WHERE id=$1`, id)
			if source == "seeded" {
				e.exec(t, `UPDATE runs SET plan_source='seeded' WHERE id=$1`, id)
			} else {
				e.exec(t, `INSERT INTO run_user_inputs(run_id,kind,body,consumed_at,applied_at) VALUES($1,'approve_plan','',now(),now())`, id)
			}
			assertPause(t, call(t, id, "pause", `{"claim_generation":4}`, e.worker), 200, true, "running")
		}
	})
	t.Run("guarded_question_replay_is_no_write_and_fenced", func(t *testing.T) {
		id := newRun(t)
		body := `{"status":"awaiting_input","claim_generation":4,"require_running":true,"open_question_id":"accepted"}`
		if rec := call(t, id, "state", body, e.worker); rec.Code != 200 {
			t.Fatalf("first question=%d %s", rec.Code, rec.Body.String())
		}
		snapshot := func() string {
			t.Helper()
			var value string
			if err := e.pool.QueryRow(ctx, `SELECT jsonb_build_object(
 'run',to_jsonb(r), 'messages',(SELECT jsonb_agg(m ORDER BY seq) FROM run_messages m WHERE run_id=r.id),
 'inputs',(SELECT jsonb_agg(i ORDER BY id) FROM run_user_inputs i WHERE run_id=r.id))::text FROM runs r WHERE id=$1`, id).Scan(&value); err != nil {
				t.Fatal(err)
			}
			return value
		}
		before := snapshot()
		for _, replay := range []string{body, strings.Replace(body, `"accepted"`, `" \u0000accepted "`, 1)} {
			if rec := call(t, id, "state", replay, e.worker); rec.Code != 200 {
				t.Fatalf("replay=%d %s", rec.Code, rec.Body.String())
			}
			if snapshot() != before {
				t.Fatal("accepted replay mutated run, accounting, messages or inputs")
			}
		}
		for _, qid := range []string{"unrelated", "", strings.Repeat("x", 1025)} {
			payload, err := json.Marshal(map[string]any{"status": "awaiting_input", "claim_generation": 4, "require_running": true, "open_question_id": qid})
			if err != nil {
				t.Fatal(err)
			}
			if rec := call(t, id, "state", string(payload), e.worker); rec.Code == 200 {
				t.Fatalf("invalid replay accepted: %q", qid)
			}
			if snapshot() != before {
				t.Fatal("refused replay mutated state")
			}
		}
		for _, change := range []string{"pause_requested_at=now(),pause_mode='wall'", "credential_switch_requested_at=now(),credential_switch_generation=4", "claim_generation=5", "claim_released_at=now()"} {
			fenced := newRun(t)
			if rec := call(t, fenced, "state", body, e.worker); rec.Code != 200 {
				t.Fatalf("first question=%d %s", rec.Code, rec.Body.String())
			}
			e.exec(t, "UPDATE runs SET "+change+" WHERE id=$1", fenced)
			if rec := call(t, fenced, "state", body, e.worker); rec.Code != 409 {
				t.Fatalf("%s replay=%d %s", change, rec.Code, rec.Body.String())
			}
		}
	})

	t.Run("require_running_guard_and_legacy_question", func(t *testing.T) {
		for _, change := range []string{"claim_generation=5", "claim_released_at=now()", "status='paused'", "pause_requested_at=now(),pause_mode='wall'", "credential_switch_requested_at=now(),credential_switch_generation=4"} {
			id := newRun(t)
			e.exec(t, "UPDATE runs SET "+change+" WHERE id=$1", id)
			rec := call(t, id, "state", `{"status":"awaiting_input","claim_generation":4,"require_running":true,"open_question_id":"late"}`, e.worker)
			if rec.Code != 409 {
				t.Fatalf("%s response=%d %s", change, rec.Code, rec.Body.String())
			}
			stale := strings.Contains(rec.Body.String(), `"disposition":"stale_claim"`)
			if stale != (change == "claim_generation=5" || change == "claim_released_at=now()") {
				t.Fatalf("%s stale disposition=%v: %s", change, stale, rec.Body.String())
			}
			run, err := q.GetRunByID(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if run.OpenQuestionID.Valid {
				t.Fatal("refused question persisted")
			}
		}
		for _, body := range []string{`{"status":"awaiting_input","require_running":true,"open_question_id":"x"}`, `{"status":"awaiting_input","require_running":true,"claim_generation":0,"open_question_id":"x"}`, `{"status":"running","require_running":true,"claim_generation":4}`} {
			if rec := call(t, newRun(t), "state", body, e.worker); rec.Code != 400 {
				t.Fatalf("validation=%d %s", rec.Code, rec.Body.String())
			}
		}
		for _, guard := range []string{"", `,"require_running":false`, `,"require_running":true`} {
			rec := call(t, newRun(t), "state", fmt.Sprintf(`{"status":"awaiting_input","claim_generation":4,"open_question_id":"normal"%s}`, guard), e.worker)
			if rec.Code != 200 {
				t.Fatalf("question=%d %s", rec.Code, rec.Body.String())
			}
		}
	})
}
