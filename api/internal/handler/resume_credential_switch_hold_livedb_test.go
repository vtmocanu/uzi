package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/config"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// TestResumeCredentialSwitchHoldLiveDB (issue #3181) drives the real ResumeRunNow handler against a
// live DB: a credential_switch_settling hold whose worker heartbeats and has not re-registered is
// refused with 409 and the settling message (distinct from "no longer paused"); once the worker
// goes stale, re-registers, or the hold is settled (credential_switch_unsettled), Resume answers
// 200, queues the run, keeps its worker and clears the hold reason.
func TestResumeCredentialSwitchHoldLiveDB(t *testing.T) {
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
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	owner := store.User{ID: uuid.New()}
	connID, repoID := uuid.New(), uuid.New()
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, owner.ID, fmt.Sprintf("hold-%s@e2e", owner.ID))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, owner.ID, []byte{0x1})
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)

	h := &Handler{q: store.New(pool), cfg: config.Config{WorkerHeartbeatStale: 45 * time.Second}}
	var iid int64 = 9000
	newWorker := func() uuid.UUID {
		w := uuid.New()
		exec(`INSERT INTO workers (id, user_id, name, token_hash, status, last_heartbeat_at, dind_register_floor)
		      VALUES ($1, $2, $3, $4, 'online', now(), now() - interval '1 hour')`, w, owner.ID, "hold-"+w.String(), w[:])
		return w
	}
	newHold := func(worker uuid.UUID, holdReason string) uuid.UUID {
		iid++
		id := uuid.New()
		exec(`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind, worker_id,
		          claim_generation, started_at, status_since, hold_reason, budget_wall_seconds)
		      VALUES ($1,$2,$3,$4,'t','d','paused','issue',$5,3, now() - interval '2 hours', now() - interval '1 minute', $6, 86400)`,
			id, owner.ID, repoID, iid, worker, holdReason)
		return id
	}
	call := func(id uuid.UUID) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/runs/"+id.String()+"/resume-now", nil)
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", id.String())
		req = req.WithContext(context.WithValue(mw.ContextWithUser(req.Context(), owner), chi.RouteCtxKey, rctx))
		rec := httptest.NewRecorder()
		h.ResumeRunNow(rec, req)
		return rec
	}
	errBody := func(rec *httptest.ResponseRecorder) string {
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		msg, _ := body["error"].(string)
		return msg
	}
	read := func(id uuid.UUID) (status, hold string, worker *uuid.UUID) {
		var hr *string
		if err := pool.QueryRow(ctx, `SELECT status, hold_reason, worker_id FROM runs WHERE id=$1`, id).Scan(&status, &hr, &worker); err != nil {
			t.Fatalf("read run: %v", err)
		}
		if hr != nil {
			hold = *hr
		}
		return
	}

	t.Run("settling hold is refused with the settling message", func(t *testing.T) {
		w := newWorker()
		id := newHold(w, "credential_switch_settling")
		rec := call(id)
		if rec.Code != http.StatusConflict {
			t.Fatalf("code = %d, want 409; body=%s", rec.Code, rec.Body.String())
		}
		if got := errBody(rec); got != workersvc.ErrRunHoldSettling.Error() {
			t.Fatalf("message = %q, want the settling message %q", got, workersvc.ErrRunHoldSettling.Error())
		}
		if status, hold, _ := read(id); status != "paused" || hold != "credential_switch_settling" {
			t.Fatalf("refused resume changed the run: %s/%s", status, hold)
		}
	})
	t.Run("not paused keeps its own message", func(t *testing.T) {
		w := newWorker()
		id := newHold(w, "credential_switch_settling")
		exec(`UPDATE runs SET status='queued', hold_reason=NULL WHERE id=$1`, id)
		rec := call(id)
		if rec.Code != http.StatusConflict || errBody(rec) == workersvc.ErrRunHoldSettling.Error() {
			t.Fatalf("code=%d message=%q, want a 409 that is not the settling message", rec.Code, errBody(rec))
		}
	})
	t.Run("stale worker lifts the fence", func(t *testing.T) {
		w := newWorker()
		id := newHold(w, "credential_switch_settling")
		exec(`UPDATE workers SET last_heartbeat_at = now() - interval '10 minutes' WHERE id=$1`, w)
		rec := call(id)
		if rec.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		status, hold, worker := read(id)
		if status != "queued" || hold != "" || worker == nil || *worker != w {
			t.Fatalf("after resume: %s hold=%q worker=%v, want queued, hold cleared, worker kept", status, hold, worker)
		}
	})
	t.Run("re-registered worker lifts the fence", func(t *testing.T) {
		w := newWorker()
		id := newHold(w, "credential_switch_settling")
		exec(`UPDATE workers SET dind_register_floor = now() WHERE id=$1`, w)
		if rec := call(id); rec.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
	})
	t.Run("deleted worker lifts the fence", func(t *testing.T) {
		w := newWorker()
		id := newHold(w, "credential_switch_settling")
		exec(`DELETE FROM workers WHERE id=$1`, w)
		if rec := call(id); rec.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
	})
	t.Run("unsettled hold resumes and clears the reason", func(t *testing.T) {
		w := newWorker()
		id := newHold(w, "credential_switch_unsettled")
		rec := call(id)
		if rec.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		status, hold, worker := read(id)
		if status != "queued" || hold != "" || worker == nil || *worker != w {
			t.Fatalf("after resume: %s hold=%q worker=%v, want queued, hold cleared, worker kept", status, hold, worker)
		}
	})
}
