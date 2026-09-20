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

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// TestWorkerRunStateWallParkedAckLiveDB drives the REAL /state handler and proves the agent-facing
// contract PRD #1497 M2's ServerWallParkedError path depends on: a fenced-out `running` heartbeat
// from a flight whose run the sweep already SERVER-PARKED at the wall (ParkRunsAtWall left it
// status='paused', hold_reason='budget_exhausted', claim_released_at set, worker_id + generation
// kept) comes back 409 carrying disposition:"stale_claim" TOGETHER with the run's status:"paused"
// and hold_reason:"budget_exhausted" on the run DTO. That trio is what the worker's reportState
// closure reads to throw ServerWallParkedError (retain clone + HOME, end non-terminal) AHEAD of the
// ordinary StaleClaimError (plain stop). No additional server change is needed — the generation
// fence already returns the locked row and runToDTO already carries status + hold_reason; this test
// PINS that so a future refactor of the stale_claim ack cannot silently break wall-park recognition.
func TestWorkerRunStateWallParkedAckLiveDB(t *testing.T) {
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

	q := store.New(pool)
	box := newHandlerTestBox(t)
	wsvc := workersvc.New(q, box, workersvc.Params{})
	wsvc.SetTxBeginner(pool) // REQUIRED: the fence opens a tx when a report carries a generation
	h := &Handler{pool: pool, q: q, box: box, wsvc: wsvc}

	userID, connID, repoID, workerID, runID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	uniq := func(id uuid.UUID) int64 { return int64(binary.BigEndian.Uint64(id[:8]) >> 1) }
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed (%s): %v", strings.Fields(strings.TrimSpace(sql))[1], err)
		}
	}
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, userID, fmt.Sprintf("wallpark-%s@e2e", userID))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, $3, $4, 'https://forge.e2e/g/r', 'main', true)`,
		repoID, connID, uniq(repoID), fmt.Sprintf("g/r-%s", repoID))
	exec(`INSERT INTO workers (id, user_id, name, token_hash) VALUES ($1, $2, 'wallpark', $3)`,
		workerID, userID, append([]byte("wallpark-"), workerID[:]...))
	// A run SERVER-PARKED at the wall (as ParkRunsAtWall leaves it): status='paused',
	// hold_reason='budget_exhausted', claim_released_at set — but worker_id + claim_generation kept,
	// so the old flight's fenced report is refused as stale while the row reads its parked status.
	exec(`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, worker_id, claim_generation, claim_released_at, hold_reason, started_at, status_since)
	      VALUES ($1, $2, $3, $4, 't', 'd', 'paused', $5, 4, now(), 'budget_exhausted', now() - interval '1 hour', now())`,
		runID, userID, repoID, uniq(runID), workerID)

	req := httptest.NewRequest(http.MethodPost, "/api/worker/runs/x/state", bytes.NewReader([]byte(`{"status":"running","claim_generation":4}`)))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", runID.String())
	reqCtx := context.WithValue(mw.ContextWithWorker(req.Context(), store.Worker{ID: workerID, UserID: userID}), chi.RouteCtxKey, rctx)
	rec := httptest.NewRecorder()
	h.WorkerRunState(rec, req.WithContext(reqCtx))

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (stale claim on a server-parked run); body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Disposition string `json:"disposition"`
		Run         struct {
			Status     string  `json:"status"`
			HoldReason *string `json:"hold_reason"`
		} `json:"run"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode ack: %v (body=%s)", err, rec.Body.String())
	}
	if body.Disposition != "stale_claim" {
		t.Fatalf("disposition = %q, want stale_claim", body.Disposition)
	}
	// The load-bearing assertion: the run DTO carries the wall-park status + hold_reason beside the
	// stale_claim disposition. Without this pair the worker cannot tell a SERVER wall park from an
	// ordinary supersede, and would run ordinary teardown (removing this worker's clone) instead of
	// retaining the work for a resume on another incarnation.
	if body.Run.Status != "paused" {
		t.Fatalf("run.status = %q, want paused (the server-side wall park)", body.Run.Status)
	}
	if body.Run.HoldReason == nil || *body.Run.HoldReason != "budget_exhausted" {
		t.Fatalf("run.hold_reason = %v, want budget_exhausted", body.Run.HoldReason)
	}
	// The stale report must not have flipped the parked run back to running.
	var status, holdReason string
	if err := pool.QueryRow(ctx, `SELECT status, COALESCE(hold_reason,'') FROM runs WHERE id = $1`, runID).Scan(&status, &holdReason); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != "paused" || holdReason != "budget_exhausted" {
		t.Fatalf("run stayed status=%q hold_reason=%q, want paused/budget_exhausted (a stale report changes nothing)", status, holdReason)
	}
}
