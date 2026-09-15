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

// TestWorkerRunStateStaleClaimDispositionLiveDB drives the REAL /state handler and proves the
// generation fence's ack contract (PRD #1247 M5, D3): a `running` report whose claim_generation
// names a RELEASED claim comes back 409 with a `disposition: "stale_claim"` field (the new worker
// reads it to STOP the flight), while the run is NOT flipped out of its released `queued` state.
// The handler's wsvc must have the tx beginner wired for the fence transaction.
func TestWorkerRunStateStaleClaimDispositionLiveDB(t *testing.T) {
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
	// Per-test-unique bigints for the fixture's UNIQUE columns, derived from the uuids (fresh per
	// run) rather than math/rand (gosec G404) — the same "derive uniqueness from uuid.New()" rule
	// .claude/rules/go.md gives for shared-DB fixtures. >>1 keeps them positive.
	uniq := func(id uuid.UUID) int64 { return int64(binary.BigEndian.Uint64(id[:8]) >> 1) }
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed (%s): %v", strings.Fields(strings.TrimSpace(sql))[1], err)
		}
	}
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, userID, fmt.Sprintf("stale-%s@e2e", userID))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, $3, $4, 'https://forge.e2e/g/r', 'main', true)`,
		repoID, connID, uniq(repoID), fmt.Sprintf("g/r-%s", repoID))
	exec(`INSERT INTO workers (id, user_id, name, token_hash) VALUES ($1, $2, 'stale', $3)`,
		workerID, userID, append([]byte("stale-"), workerID[:]...))
	// A run RELEASED at generation 4 (as ReleaseCredentialSwitch leaves it): queued, claim_released_at set.
	exec(`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, worker_id, claim_generation, claim_released_at)
	      VALUES ($1, $2, $3, $4, 't', 'd', 'queued', $5, 4, now())`, runID, userID, repoID, uniq(runID), workerID)

	req := httptest.NewRequest(http.MethodPost, "/api/worker/runs/x/state", bytes.NewReader([]byte(`{"status":"running","claim_generation":4}`)))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", runID.String())
	reqCtx := context.WithValue(mw.ContextWithWorker(req.Context(), store.Worker{ID: workerID, UserID: userID}), chi.RouteCtxKey, rctx)
	rec := httptest.NewRecorder()
	h.WorkerRunState(rec, req.WithContext(reqCtx))

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (stale claim); body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Disposition string          `json:"disposition"`
		Run         json.RawMessage `json:"run"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode ack: %v (body=%s)", err, rec.Body.String())
	}
	if body.Disposition != "stale_claim" {
		t.Fatalf("disposition = %q, want stale_claim", body.Disposition)
	}
	if len(body.Run) == 0 {
		t.Fatal("the stale_claim ack must still carry the run")
	}
	// The stale report must not have flipped the released run back to running.
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM runs WHERE id = $1`, runID).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != "queued" {
		t.Fatalf("run status = %q, want it to STAY queued (a stale report changes nothing)", status)
	}
}
