package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestResumeRunNowDispatchLiveDB pins the widened resume endpoint (PRD #1190 M1, Decision 14):
// ResumeRunNow READS the run first, then dispatches on status — pool_wait promotes, paused
// resumes, worker-requeue exhaustion starts another episode, and other statuses return 409.
// A foreign/absent run is 404. It
// exercises the real handler against a live DB (the store queries are the load-bearing half).
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.
func TestResumeRunNowDispatchLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via the store integration runner for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()

	owner := store.User{ID: uuid.New()}
	connID, repoID := uuid.New(), uuid.New()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, owner.ID, fmt.Sprintf("resume-%s@e2e", owner.ID))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, owner.ID, []byte{0x1})
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)

	h := &Handler{q: store.New(pool)}
	var iid int64
	newRun := func(status string) uuid.UUID {
		iid++
		id := uuid.New()
		exec(`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind, started_at)
		      VALUES ($1,$2,$3,$4,'t','d',$5,'issue', now() - interval '2 hours')`, id, owner.ID, repoID, iid, status)
		return id
	}
	call := func(id uuid.UUID, user store.User) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/runs/"+id.String()+"/resume-now", nil)
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", id.String())
		req = req.WithContext(context.WithValue(mw.ContextWithUser(req.Context(), user), chi.RouteCtxKey, rctx))
		rec := httptest.NewRecorder()
		h.ResumeRunNow(rec, req)
		return rec
	}
	statusOf := func(id uuid.UUID) string {
		var s string
		if err := pool.QueryRow(ctx, `SELECT status FROM runs WHERE id=$1`, id).Scan(&s); err != nil {
			t.Fatalf("read status: %v", err)
		}
		return s
	}
	// newHold seeds a PAUSED issue run carrying hold_reason, started 3h ago and parked 1m ago (so a
	// paused row's active elapsed is ~3h). Remaining budget for a paused row is
	// total - (status_since - started_at - budget_paused_seconds): budgetPaused banks the elapsed, so
	// a large budgetPaused leaves budget while a zero one leaves the run out of time.
	newHold := func(holdReason string, budgetWall, budgetPaused int) uuid.UUID {
		iid++
		id := uuid.New()
		exec(`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind, started_at, status_since, hold_reason, budget_wall_seconds, budget_paused_seconds)
		      VALUES ($1,$2,$3,$4,'t','d','paused','issue', now() - interval '3 hours', now() - interval '1 minute', $5, $6, $7)`,
			id, owner.ID, repoID, iid, holdReason, budgetWall, budgetPaused)
		return id
	}
	errBody := func(rec *httptest.ResponseRecorder) string {
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		msg, _ := body["error"].(string)
		return msg
	}

	t.Run("paused resumes to queued and writes a resume audit row", func(t *testing.T) {
		id := newRun("paused")
		rec := call(id, owner)
		if rec.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		if statusOf(id) != "queued" {
			t.Fatalf("resumed status = %q, want queued", statusOf(id))
		}
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM run_user_inputs WHERE run_id=$1 AND kind='resume'`, id).Scan(&n); err != nil {
			t.Fatalf("count resume audit rows: %v", err)
		}
		if n != 1 {
			t.Fatalf("resume audit rows = %d, want 1", n)
		}
	})

	t.Run("worker requeue exhaustion with a checkpoint allows owner resume", func(t *testing.T) {
		id := newRun("running")
		worker := uuid.New()
		tip := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		exec("INSERT INTO workers (id,user_id,name,token_hash) VALUES ($1,$2,'exhaustion-resume',$3)", worker, owner.ID, worker[:])
		exec("UPDATE runs SET worker_id=$2,claim_generation=2,requeue_count=2,budget_wall_seconds=86400,checkpoint_tip=$3,checkpoint_tip_at=now() WHERE id=$1", id, worker, tip)
		if _, err := h.q.FailWorkerRunsOverCap(ctx, store.FailWorkerRunsOverCapParams{
			WorkerID: pgtype.UUID{Bytes: worker, Valid: true}, MaxRequeues: 1,
			FailureReason: pgtype.Text{String: "worker requeue budget exhausted", Valid: true},
		}); err != nil {
			t.Fatal(err)
		}
		// Reach the hold through its real writer. The current cause CHECK must not
		// be bypassed by inserting a fabricated recovery_wait fixture.
		var status, cause string
		var finished bool
		if err := pool.QueryRow(ctx, "SELECT status,COALESCE(recovery_wait_cause,''),finished_at IS NOT NULL FROM runs WHERE id=$1", id).Scan(&status, &cause, &finished); err != nil {
			t.Fatal(err)
		}
		if status != "recovery_wait" || cause != "worker_requeue_exhausted" || finished {
			t.Fatalf("exhaustion before owner resume = status=%q cause=%q finished=%v; want recovery_wait/worker_requeue_exhausted/nonterminal", status, cause, finished)
		}
		foreign := call(id, store.User{ID: uuid.New()})
		if foreign.Code != http.StatusNotFound || statusOf(id) != "recovery_wait" {
			t.Fatalf("foreign resume code=%d status=%s, want 404/recovery_wait", foreign.Code, statusOf(id))
		}
		rec := call(id, owner)
		if rec.Code != http.StatusOK {
			t.Fatalf("owner exhaustion resume code=%d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		if statusOf(id) != "queued" {
			t.Fatalf("owner exhaustion resume status=%s, want queued", statusOf(id))
		}
		var retainedTip string
		var inputs int
		if err := pool.QueryRow(ctx, "SELECT checkpoint_tip,(SELECT count(*) FROM run_user_inputs WHERE run_id=$1 AND kind='resume') FROM runs WHERE id=$1", id).Scan(&retainedTip, &inputs); err != nil {
			t.Fatal(err)
		}
		if retainedTip != tip || inputs != 1 {
			t.Fatalf("resumed checkpoint=%q audit rows=%d, want %q/1", retainedTip, inputs, tip)
		}
	})

	t.Run("ordinary recovery_wait still refuses generic owner resume", func(t *testing.T) {
		id := newRun("recovery_wait")
		exec("UPDATE runs SET recovery_wait_cause='forge_unreachable' WHERE id=$1", id)
		rec := call(id, owner)
		if rec.Code != http.StatusConflict || errBody(rec) != "run is recovery_wait" || statusOf(id) != "recovery_wait" {
			t.Fatalf("ordinary recovery hold resume code=%d body=%s status=%s, want 409/unchanged", rec.Code, rec.Body.String(), statusOf(id))
		}
	})

	t.Run("pool_wait promotes to queued", func(t *testing.T) {
		id := newRun("pool_wait")
		rec := call(id, owner)
		if rec.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		if statusOf(id) != "queued" {
			t.Fatalf("promoted status = %q, want queued", statusOf(id))
		}
	})

	t.Run("running is a 409 naming the status", func(t *testing.T) {
		id := newRun("running")
		rec := call(id, owner)
		if rec.Code != http.StatusConflict {
			t.Fatalf("code = %d, want 409; body=%s", rec.Code, rec.Body.String())
		}
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if msg, _ := body["error"].(string); msg != "run is running" {
			t.Fatalf("409 message = %q, want \"run is running\"", msg)
		}
		if statusOf(id) != "running" {
			t.Fatal("a refused resume must not move the run")
		}
	})

	t.Run("a foreign run is 404 before any write", func(t *testing.T) {
		id := newRun("paused")
		rec := call(id, store.User{ID: uuid.New()})
		if rec.Code != http.StatusNotFound {
			t.Fatalf("code = %d, want 404; body=%s", rec.Code, rec.Body.String())
		}
		if statusOf(id) != "paused" {
			t.Fatal("a foreign resume must not move the run")
		}
	})

	// PRD #1497 M1: the two wall-park 409s that resume.go's `case "paused"` returns. Both are distinct
	// from the generic running→409 default the subtests above cover, and each asserts the EXACT body
	// text resume.go produces (copied literally). The positive control below keeps them non-vacuous.
	t.Run("a completion_blocked hold is a 409 naming the decision endpoint (D6)", func(t *testing.T) {
		// resume.go reads hold_reason BEFORE ResumePausedRun: a completion_blocked hold resumes ONLY
		// through POST /api/runs/{id}/completion/decision, so this generic resume 409s naming it.
		// (Removing that hold_reason branch would silently resume the hold to queued instead.)
		id := newHold("completion_blocked", 3600, 0)
		rec := call(id, owner)
		if rec.Code != http.StatusConflict {
			t.Fatalf("code = %d, want 409; body=%s", rec.Code, rec.Body.String())
		}
		want := "this run is blocked on a completion decision; resolve it at POST /api/runs/{id}/completion/decision"
		if msg := errBody(rec); msg != want {
			t.Fatalf("409 message = %q, want %q", msg, want)
		}
		if statusOf(id) != "paused" {
			t.Fatal("a refused resume must not move the completion hold")
		}
	})

	t.Run("a budget_exhausted hold with no remaining budget is a 409 naming extend (D7)", func(t *testing.T) {
		// budgetPaused 0 → the ~3h active elapsed exceeds the 1h wall → ResumePausedRun's
		// remaining-budget guard refuses (pgx.ErrNoRows), and resume.go answers the out-of-time 409.
		id := newHold("budget_exhausted", 3600, 0)
		rec := call(id, owner)
		if rec.Code != http.StatusConflict {
			t.Fatalf("code = %d, want 409; body=%s", rec.Code, rec.Body.String())
		}
		want := fmt.Sprintf("this run is out of time; extend it to resume: uzi run extend %s --by 2h", id)
		if msg := errBody(rec); msg != want {
			t.Fatalf("409 message = %q, want %q", msg, want)
		}
		if statusOf(id) != "paused" {
			t.Fatal("an out-of-time budget_exhausted hold must stay paused")
		}
	})

	t.Run("a credential_disabled hold is a 409 naming Settings (PRD #1732 D12)", func(t *testing.T) {
		// ResumePausedRun refuses a credential_disabled hold outright (only enabling the credential
		// resumes it), so resume.go must name that fix rather than the "no longer paused" race text.
		// Budget is ample, so the refusal is the hold itself, not the D7 remaining-budget guard.
		id := newHold("credential_disabled", 3600, 3*3600)
		rec := call(id, owner)
		if rec.Code != http.StatusConflict {
			t.Fatalf("code = %d, want 409; body=%s", rec.Code, rec.Body.String())
		}
		want := "credential is disabled; enable it in Settings"
		if msg := errBody(rec); msg != want {
			t.Fatalf("409 message = %q, want %q", msg, want)
		}
		if statusOf(id) != "paused" {
			t.Fatal("a refused resume must not move the credential_disabled hold")
		}
	})

	// Positive control: a budget_exhausted hold WITH remaining budget resumes 200/queued, so neither
	// 409 above is vacuous — the endpoint genuinely resumes a budget_exhausted park when it can.
	t.Run("a budget_exhausted hold with remaining budget resumes to queued", func(t *testing.T) {
		// budgetPaused banks the whole ~3h elapsed → remaining budget > 0 → ResumePausedRun resumes.
		id := newHold("budget_exhausted", 3600, 3*3600)
		rec := call(id, owner)
		if rec.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		if statusOf(id) != "queued" {
			t.Fatalf("resumed status = %q, want queued", statusOf(id))
		}
	})
}
