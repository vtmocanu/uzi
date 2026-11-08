package handler

import (
	"bytes"
	"context"
	"encoding/binary"
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

// TestWorkerRunStateRetainedCapacityOnlyLiveDB drives the REAL /state handler with the issue
// #4046 marker, so the wire contract is executed end to end: the strict decoder accepts
// retained_capacity_only, a misplaced marker is a 400 that writes nothing, and a valid marked
// retained_recovery_exhausted park below the cap is held uncounted with no retry deadline.
func TestWorkerRunStateRetainedCapacityOnlyLiveDB(t *testing.T) {
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
	wsvc := workersvc.New(q, box, workersvc.Params{CustodyHoldLimit: defaultCustodyLimit(), RunRetainedRecoveryMaxParks: 3})
	wsvc.SetTxBeginner(pool)
	h := &Handler{pool: pool, q: q, box: box, wsvc: wsvc}

	userID, connID, repoID, workerID, runID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	uniq := func(id uuid.UUID) int64 { return int64(binary.BigEndian.Uint64(id[:8]) >> 1) }
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed (%s): %v", strings.Fields(strings.TrimSpace(sql))[1], err)
		}
	}
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, userID, fmt.Sprintf("capacity-%s@e2e", userID))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, $3, $4, 'https://forge.e2e/g/r', 'main', true)`,
		repoID, connID, uniq(repoID), fmt.Sprintf("g/r-%s", repoID))
	exec(`INSERT INTO workers (id, user_id, name, token_hash) VALUES ($1, $2, 'capacity', $3)`,
		workerID, userID, append([]byte("capacity-"), workerID[:]...))
	exec(`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, worker_id, claim_generation, retained_park_count, started_at, status_since)
	      VALUES ($1, $2, $3, $4, 't', 'd', 'running', $5, 3, 1, now() - interval '10 minutes', now())`,
		runID, userID, repoID, uniq(runID), workerID)

	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/worker/runs/x/state", bytes.NewReader([]byte(body)))
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", runID.String())
		reqCtx := context.WithValue(mw.ContextWithWorker(req.Context(), store.Worker{ID: workerID, UserID: userID}), chi.RouteCtxKey, rctx)
		rec := httptest.NewRecorder()
		h.WorkerRunState(rec, req.WithContext(reqCtx))
		return rec
	}
	read := func() store.Run {
		t.Helper()
		r, err := q.GetRunByID(ctx, runID)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}

	if rec := post(`{"status":"recovery_wait","recovery_cause":"retained_recovery_adoption_failed","retained_capacity_only":true,"claim_generation":3}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("misplaced marker status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if r := read(); r.Status != "running" || r.RetainedParkCount != 1 || r.ClaimReleasedAt.Valid {
		t.Fatalf("refused marker wrote: status=%s count=%d released=%v", r.Status, r.RetainedParkCount, r.ClaimReleasedAt.Valid)
	}

	rec := post(`{"status":"recovery_wait","recovery_cause":"retained_recovery_exhausted","retained_capacity_only":true,"claim_generation":3}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("marked park status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"recovery_wait"`) {
		t.Fatalf("ack does not confirm recovery_wait: %s", rec.Body.String())
	}
	r := read()
	if r.Status != "recovery_wait" || r.RecoveryWaitCause.String != "retained_recovery_exhausted" ||
		r.RetainedParkCount != 1 || r.RecoveryRetryNotBefore.Valid || !r.ClaimReleasedAt.Valid {
		t.Fatalf("marked park must hold uncounted: status=%s cause=%+v count=%d retry=%+v", r.Status, r.RecoveryWaitCause, r.RetainedParkCount, r.RecoveryRetryNotBefore)
	}
}
