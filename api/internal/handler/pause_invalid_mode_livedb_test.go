package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// TestCreateRunInputPauseInvalidModeLiveDB (PRD #1190 M1) pins the invalid-pause-mode 400: a
// POST /inputs {"kind":"pause","body":"<not milestone|now>"} on a running pausable run is a
// CALLER error, so it maps to 400 with the mode message — not the 500 the default arm would
// return before ErrInvalidPauseMode got its own case. The reject happens before any pending-
// pause write, so the run row is left untouched.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.
func TestCreateRunInputPauseInvalidModeLiveDB(t *testing.T) {
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
	connID, repoID, runID := uuid.New(), uuid.New(), uuid.New()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, owner.ID, fmt.Sprintf("pause-mode-%s@e2e", owner.ID))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, owner.ID, []byte{0x1})
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)
	// A running, non-interactive issue run: pausable, so the ONLY reason to refuse is the mode.
	exec(`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind, started_at)
	      VALUES ($1,$2,$3,1,'t','d','running','issue', now() - interval '2 hours')`, runID, owner.ID, repoID)

	h := &Handler{wsvc: workersvc.New(store.New(pool), nil, workersvc.Params{})}
	call := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/runs/"+runID.String()+"/inputs", strings.NewReader(body))
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", runID.String())
		req = req.WithContext(context.WithValue(mw.ContextWithUser(req.Context(), owner), chi.RouteCtxKey, rctx))
		rec := httptest.NewRecorder()
		h.CreateRunInput(rec, req)
		return rec
	}

	rec := call(`{"kind":"pause","body":"soon"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal body: %v; raw=%s", err, rec.Body.String())
	}
	msg, _ := resp["error"].(string)
	if !strings.Contains(msg, "pause mode must be 'milestone' or 'now'") {
		t.Fatalf("400 message = %q, want the mode message", msg)
	}

	// The invalid mode is rejected BEFORE any pending-pause write, so the run row is untouched.
	var pendingCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM runs WHERE id=$1 AND pause_requested_at IS NOT NULL`, runID).Scan(&pendingCount); err != nil {
		t.Fatalf("read pause_requested_at: %v", err)
	}
	if pendingCount != 0 {
		t.Fatal("a refused pause must not stamp pause_requested_at")
	}
}
