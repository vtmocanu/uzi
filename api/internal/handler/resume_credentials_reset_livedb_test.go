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

// TestResumeRunNowRefusesCredentialsResetHoldLiveDB (PRD #3989 M3): the owner's Resume of a run held
// on credentials_reset is a 409 naming the fix, and changes nothing: the run stays paused on the
// same hold with every column untouched. Only the credential promoter releases that hold. A
// budget_exhausted hold with budget left resumes in the same fixture, so the refusal is the hold.
//
// MUTATION: delete the credentials_reset term from ResumePausedRun (runtime.sql) and the run resumes
// to queued, failing the 409 and unchanged-row assertions.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.
func TestResumeRunNowRefusesCredentialsResetHoldLiveDB(t *testing.T) {
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
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, owner.ID, fmt.Sprintf("reset-hold-%s@e2e", owner.ID))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, owner.ID, []byte{0x1})
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)

	h := &Handler{q: store.New(pool)}
	var iid int64
	newHold := func(reason string) uuid.UUID {
		iid++
		id := uuid.New()
		exec(`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind, started_at,
		        budget_wall_seconds, budget_paused_seconds, hold_reason, status_since)
		      VALUES ($1, $2, $3, $4, 't', 'd', 'paused', 'issue', now() - interval '3 hours', 3600, 10800, $5, now() - interval '10 minutes')`,
			id, owner.ID, repoID, iid, reason)
		return id
	}
	snapshot := func(id uuid.UUID) string {
		t.Helper()
		var v string
		if err := pool.QueryRow(ctx, `SELECT to_jsonb(r)::text FROM runs r WHERE id = $1`, id).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
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

	id := newHold("credentials_reset")
	before := snapshot(id)
	rec := call(id)
	if rec.Code != http.StatusConflict {
		t.Fatalf("code = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	want := "credentials were reset; add a credential in Settings and the run resumes automatically"
	if body.Error != want {
		t.Fatalf("409 message = %q, want %q", body.Error, want)
	}
	if after := snapshot(id); after != before {
		t.Fatalf("a refused resume changed the run:\nbefore %s\nafter  %s", before, after)
	}
	var status, hold string
	if err := pool.QueryRow(ctx, `SELECT status, hold_reason FROM runs WHERE id = $1`, id).Scan(&status, &hold); err != nil ||
		status != "paused" || hold != "credentials_reset" {
		t.Fatalf("status=%q hold=%q err=%v, want paused on credentials_reset", status, hold, err)
	}
	var audits int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM run_user_inputs WHERE run_id = $1 AND kind = 'resume'`, id).Scan(&audits); err != nil || audits != 0 {
		t.Fatalf("resume audit rows = %d (err %v), want 0 for a refused resume", audits, err)
	}

	// Positive control: the same fixture resumes a hold that may be resumed.
	ctl := newHold("budget_exhausted")
	if rec := call(ctl); rec.Code != http.StatusOK {
		t.Fatalf("control code = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}
