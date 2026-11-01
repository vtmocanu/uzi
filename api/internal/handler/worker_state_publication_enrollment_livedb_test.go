package handler

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/capability"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// TestWorkerRunStatePublicationEnrollmentChangedLiveDB drives the REAL /state handler through the
// race the service guards: the report reads the run as not enrolled, then waits on the run row
// while an enrollment commits. ErrPublicationEnrollmentChanged must surface as a retryable 503
// carrying reason publication_enrollment_changed (not a 409 no-op ack, not a 500), and the run
// must be unchanged. WorkerRunState maps the error inline, so the handler is the only seam.
func TestWorkerRunStatePublicationEnrollmentChangedLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)

	userID, connID, repoID, workerID, runID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	app := "cdr1324-h-" + runID.String()
	// The handler's own pool, so its backend is identifiable by application_name.
	hpool, err := store.OpenPool(ctx, dsn, func(c *pgxpool.Config) { c.ConnConfig.RuntimeParams["application_name"] = app })
	if err != nil {
		t.Fatalf("open handler pool: %v", err)
	}
	t.Cleanup(hpool.Close)
	q := store.New(hpool)
	box := newHandlerTestBox(t)
	wsvc := workersvc.New(q, box, workersvc.Params{CustodyHoldLimit: defaultCustodyLimit()})
	wsvc.SetTxBeginner(hpool)
	h := &Handler{pool: hpool, q: q, box: box, wsvc: wsvc}

	uniq := func(id uuid.UUID) int64 { return int64(binary.BigEndian.Uint64(id[:8]) >> 1) }
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed (%s): %v", strings.Fields(strings.TrimSpace(sql))[1], err)
		}
	}
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, userID, fmt.Sprintf("pubenr-%s@e2e", userID))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, $3, $4, 'https://forge.e2e/g/r', 'main', true)`,
		repoID, connID, uniq(repoID), fmt.Sprintf("g/r-%s", repoID))
	exec(`INSERT INTO workers (id, user_id, name, token_hash, protocol_capabilities) VALUES ($1, $2, 'pubenr', $3, $4)`,
		workerID, userID, append([]byte("pubenr-"), workerID[:]...), []string{capability.PublicationResumeV1})
	exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id, claim_generation, branch, started_at)
	      VALUES ($1, $2, $3, 'issue', $4, 't', 'd', 'running', $5, 1, 'agent/issue-1', now())`, runID, userID, repoID, uniq(runID), workerID)

	runRow := func() string {
		var s string
		if err := pool.QueryRow(ctx, `SELECT to_jsonb(r)::text FROM runs r WHERE id=$1`, runID).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := runRow()

	// The holder enrolls the run (worker lock, run lock, opt-in, insert), uncommitted.
	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	hq := store.New(holder)
	if _, err = hq.GetWorkerForUpdate(ctx, workerID); err != nil {
		t.Fatal(err)
	}
	if err = hq.OptInRunFences(ctx); err != nil {
		t.Fatal(err)
	}
	var locked string
	if err = holder.QueryRow(ctx, `SELECT id::text FROM runs WHERE id=$1 FOR UPDATE`, runID).Scan(&locked); err != nil {
		t.Fatal(err)
	}
	intent, err := json.Marshal(map[string]any{
		"run_id": runID, "repo_id": repoID, "branch": "agent/issue-1", "claim_generation": 1,
		"original_head": strings.Repeat("a", 40), "floor": map[string]any{"kind": "absent"}, "candidate": nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = hq.EnrollPublicationResume(ctx, store.EnrollPublicationResumeParams{
		RunID: runID, ClaimGeneration: 1, WorkerID: workerID, Intent: intent,
	}); err != nil {
		t.Fatal(err)
	}

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/api/worker/runs/x/state", bytes.NewReader([]byte(`{"status":"running","claim_generation":1}`)))
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", runID.String())
		reqCtx := context.WithValue(mw.ContextWithWorker(req.Context(), store.Worker{ID: workerID, UserID: userID}), chi.RouteCtxKey, rctx)
		rec := httptest.NewRecorder()
		h.WorkerRunState(rec, req.WithContext(reqCtx))
		done <- rec
	}()
	deadline := time.Now().Add(15 * time.Second)
	for blocked := false; !blocked; {
		if time.Now().After(deadline) {
			t.Fatalf("no backend named %q reached a Lock wait", app)
		}
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE application_name=$1 AND wait_event_type='Lock'`, app).Scan(&n); err != nil {
			t.Fatal(err)
		}
		blocked = n > 0
		if !blocked {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if err = holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var rec *httptest.ResponseRecorder
	select {
	case rec = <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the report did not return after the enrollment committed")
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Error  string `json:"error"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
	}
	if body.Reason != "publication_enrollment_changed" || body.Error == "" {
		t.Fatalf("body=%s, want reason publication_enrollment_changed", rec.Body.String())
	}
	if after := runRow(); after != before {
		t.Fatalf("the refused report changed the run row:\nbefore=%s\nafter =%s", before, after)
	}
}
