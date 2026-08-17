package handler

import (
	"bytes"
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
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/config"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// TestSetRunPriorityLiveDB exercises PATCH /api/runs/{id}/priority (PRD #320 M3 / D6/D7)
// end to end against a REAL Postgres — the four paths the fake-store harness cannot reach
// because SetRunPriority, GetRunByIDForUser and RunPriorityClass all go through the
// concrete Handler.q (there is no fake-store seam here):
//   - owner expedites a QUEUED run → 200, priority = 2, DTO.priority == "expedited";
//   - owner undoes → 200, priority NULL, DTO.priority back to "normal";
//   - a NON-owner → 404 (owner-scoped SQL predicate, never 403);
//   - a NON-queued (running) run → 409 (ordering only matters before the claim).
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; ./e2e/run-store-it.sh
// and the test:api-store-it CI job provide one and sweep this package for the LiveDB suffix.
func TestSetRunPriorityLiveDB(t *testing.T) {
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
	h := &Handler{
		pool: pool,
		q:    q,
		box:  box,
		cfg:  config.Config{WorkerBackgroundGrace: 15 * time.Minute},
		wsvc: workersvc.New(q, box, workersvc.Params{}),
	}

	owner := uuid.New()
	stranger := uuid.New()
	connID, repoID := uuid.New(), uuid.New()
	queuedID, runningID := uuid.New(), uuid.New()

	mustExecT(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		owner, fmt.Sprintf("prio-owner-%s@e2e", owner))
	mustExecT(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		stranger, fmt.Sprintf("prio-stranger-%s@e2e", stranger))
	t.Cleanup(func() {
		mustExecT(ctx, t, pool, `DELETE FROM users WHERE id IN ($1, $2)`, owner, stranger)
	})
	mustExecT(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, owner, []byte{0x1})
	mustExecT(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)
	mustExecT(ctx, t, pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind)
		 VALUES ($1, $2, $3, 1, 't', 'd', 'queued', 'issue')`, queuedID, owner, repoID)
	mustExecT(ctx, t, pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind)
		 VALUES ($1, $2, $3, 2, 't', 'd', 'running', 'issue')`, runningID, owner, repoID)

	// --- owner expedites the queued run ---
	rec := patchPriority(h, owner, queuedID, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("expedite: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if cls := decodeRunPriority(t, rec); cls != "expedited" {
		t.Fatalf("expedite: DTO.priority = %q, want expedited", cls)
	}
	if p := dbPriority(ctx, t, pool, queuedID); !p.Valid || p.Int16 != 2 {
		t.Fatalf("expedite: runs.priority = %+v, want {2 true}", p)
	}

	// --- owner undoes: priority clears to NULL, class back to the kind default ---
	rec = patchPriority(h, owner, queuedID, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("undo: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if cls := decodeRunPriority(t, rec); cls != "normal" {
		t.Fatalf("undo: DTO.priority = %q, want normal", cls)
	}
	if p := dbPriority(ctx, t, pool, queuedID); p.Valid {
		t.Fatalf("undo: runs.priority = %+v, want NULL (Valid=false)", p)
	}

	// --- a non-owner gets 404, never 403 (owner-scoped SQL predicate) ---
	rec = patchPriority(h, stranger, queuedID, true)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("non-owner: status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	// The stranger's rejected expedite must NOT have written the row.
	if p := dbPriority(ctx, t, pool, queuedID); p.Valid {
		t.Fatalf("non-owner: runs.priority = %+v, want NULL (foreign write must be a no-op)", p)
	}

	// --- a non-queued (running) run gets 409 ---
	rec = patchPriority(h, owner, runningID, true)
	if rec.Code != http.StatusConflict {
		t.Fatalf("non-queued: status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if p := dbPriority(ctx, t, pool, runningID); p.Valid {
		t.Fatalf("non-queued: runs.priority = %+v, want NULL (queued-only write must be a no-op)", p)
	}
}

// patchPriority issues PATCH /api/runs/{id}/priority as user with {"expedite": v}.
func patchPriority(h *Handler, user, runID uuid.UUID, expedite bool) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]bool{"expedite": expedite})
	req := httptest.NewRequest(http.MethodPatch, "/api/runs/x/priority", bytes.NewReader(body))
	ctx := mw.ContextWithUser(req.Context(), store.User{ID: user})
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", runID.String())
	ctx = context.WithValue(ctx, chi.RouteCtxKey, rctx)
	rec := httptest.NewRecorder()
	h.SetRunPriority(rec, req.WithContext(ctx))
	return rec
}

// decodeRunPriority pulls run.priority out of the {"run": {...}} envelope.
func decodeRunPriority(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var out struct {
		Run struct {
			Priority string `json:"priority"`
		} `json:"run"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode run envelope: %v (body=%s)", err, rec.Body.String())
	}
	return out.Run.Priority
}

// dbPriority reads runs.priority straight from the row, so the test asserts the write
// landed (or did not) independently of the DTO the handler echoed.
func dbPriority(ctx context.Context, t *testing.T, pool *pgxpool.Pool, runID uuid.UUID) pgtype.Int2 {
	t.Helper()
	var p pgtype.Int2
	if err := pool.QueryRow(ctx, `SELECT priority FROM runs WHERE id = $1`, runID).Scan(&p); err != nil {
		t.Fatalf("read runs.priority: %v", err)
	}
	return p
}
