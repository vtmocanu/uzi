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

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestResumeRunNowDispatchLiveDB pins the widened resume endpoint (PRD #1190 M1, Decision 14):
// ResumeRunNow READS the run first, then dispatches on status — pool_wait promotes, paused
// resumes, everything else is a 409 naming the status — and a foreign/absent run is 404. It
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
}
