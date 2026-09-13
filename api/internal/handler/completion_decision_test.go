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
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/config"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// TestCompletionDecisionHandlerStatusMappingLiveDB pins the HTTP status mapping for the widened
// partial/accept request at POST /api/runs/{id}/completion/decision (PRD #1227 M1) end to end against
// a REAL Postgres — the layer the DTO-projection unit test cannot reach because DecideCompletion,
// GetRun and the FOR UPDATE lock all go through the concrete Handler.wsvc/pool.
//
// For BOTH partial and accept it proves:
//   - a FOREIGN caller → 404 (the security fix: partial/accept is owner-scoped, so a foreign caller —
//     including a read-only admin_ro uza_ Bearer that keeps IsAdmin=true — is hidden, never writes);
//   - a missing/empty reason → 400 (handler shape validation, before the service);
//   - an unknown milestone/criterion id → 400 (ErrCompletionDecisionInvalid);
//   - a stale fenced contract_revision → 409 (ErrCompletionRevisionConflict).
//
// Every case is an error path (none commits), so the seeded run stays at revision 1 and is reused;
// the foreign cases additionally assert the run is unchanged (no owner-bypass write).
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; ./e2e/run-store-it.sh and the
// test:api-store-it CI job provide one and sweep this package for the LiveDB suffix.
func TestCompletionDecisionHandlerStatusMappingLiveDB(t *testing.T) {
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
	wsvc.SetTxBeginner(pool)            // partial/accept runs a transaction under the FOR UPDATE lock.
	wsvc.SetBackground(func(func()) {}) // no detached goroutine may outlive the test's pool.
	h := &Handler{
		pool: pool,
		q:    q,
		box:  box,
		cfg:  config.Config{WorkerBackgroundGrace: 15 * time.Minute},
		wsvc: wsvc,
	}

	owner := uuid.New()
	stranger := uuid.New()
	connID, repoID, runID := uuid.New(), uuid.New(), uuid.New()

	mustExecT(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		owner, fmt.Sprintf("cd-owner-%s@e2e", owner))
	mustExecT(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		stranger, fmt.Sprintf("cd-stranger-%s@e2e", stranger))
	t.Cleanup(func() {
		mustExecT(ctx, t, pool, `DELETE FROM users WHERE id IN ($1, $2)`, owner, stranger)
	})
	mustExecT(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, owner, []byte{0x1})
	mustExecT(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)

	// A frozen, completion-blocked run (contract_revision=1, milestones m1/m2, none completed), paused
	// on the completion hold so a partial/accept could resume it — though every case here errors out
	// before the resume.
	contract := []byte(`{"profile":"structural","revision":1,"criteria":[` +
		`{"id":"m1.c1","milestone_id":"m1","text":"m1","audit":null,"finding_ids":[]},` +
		`{"id":"m2.c1","milestone_id":"m2","text":"m2","audit":null,"finding_ids":[]}]}`)
	frozen := []byte(`[{"id":"m1","title":"m1"},{"id":"m2","title":"m2"}]`)
	mustExecT(ctx, t, pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind,
		     completion_contract_version, contract_revision, completion_contract, milestones_frozen, milestones_completed,
		     hold_reason, hold_captured_head, completion_attempts)
		 VALUES ($1, $2, $3, 1, 't', 'd', 'paused', 'issue', 1, 1, $4, $5, '[]', 'completion_blocked', 'capturedhead1', 1)`,
		runID, owner, repoID, contract, frozen)

	cases := []struct {
		name string
		user uuid.UUID
		req  completionDecisionRequest
		want int
	}{
		// partial
		{"partial foreign caller is 404", stranger,
			completionDecisionRequest{Decision: "partial", Keep: []string{"m1"}, Reason: "r", ContractRevision: 1}, http.StatusNotFound},
		{"partial empty reason is 400", owner,
			completionDecisionRequest{Decision: "partial", Keep: []string{"m1"}, Reason: "   ", ContractRevision: 1}, http.StatusBadRequest},
		{"partial unknown milestone is 400", owner,
			completionDecisionRequest{Decision: "partial", Keep: []string{"zzz"}, Reason: "r", ContractRevision: 1}, http.StatusBadRequest},
		{"partial stale revision is 409", owner,
			completionDecisionRequest{Decision: "partial", Keep: []string{"m1"}, Reason: "r", ContractRevision: 7}, http.StatusConflict},
		// accept
		{"accept foreign caller is 404", stranger,
			completionDecisionRequest{Decision: "accept", Criteria: []string{"m1.c1"}, Reason: "r", ContractRevision: 1}, http.StatusNotFound},
		{"accept empty reason is 400", owner,
			completionDecisionRequest{Decision: "accept", Criteria: []string{"m1.c1"}, Reason: "", ContractRevision: 1}, http.StatusBadRequest},
		{"accept unknown criterion is 400", owner,
			completionDecisionRequest{Decision: "accept", Criteria: []string{"zzz.c1"}, Reason: "r", ContractRevision: 1}, http.StatusBadRequest},
		{"accept stale revision is 409", owner,
			completionDecisionRequest{Decision: "accept", Criteria: []string{"m1.c1"}, Reason: "r", ContractRevision: 7}, http.StatusConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := postCompletionDecision(t, h, tc.user, runID, tc.req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tc.want, rec.Body.String())
			}
			// A foreign caller must have written nothing: the revision stays at 1.
			if tc.user == stranger {
				if rev := dbContractRevision(ctx, t, pool, runID); rev != 1 {
					t.Fatalf("a foreign caller must not write; contract_revision = %d, want 1", rev)
				}
			}
		})
	}
}

// postCompletionDecision issues POST /api/runs/{id}/completion/decision as user with the given body,
// calling the handler method directly with a user context and the chi {id} route param.
func postCompletionDecision(t *testing.T, h *Handler, user, runID uuid.UUID, req completionDecisionRequest) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	r := httptest.NewRequest(http.MethodPost, "/api/runs/x/completion/decision", bytes.NewReader(body))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", runID.String())
	r = r.WithContext(context.WithValue(mw.ContextWithUser(r.Context(), store.User{ID: user}), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()
	h.ContinueCompletionDecision(rec, r)
	return rec
}

// dbContractRevision reads runs.contract_revision straight from the row so a test asserts a write
// landed (or did not) independently of the DTO the handler echoed.
func dbContractRevision(ctx context.Context, t *testing.T, pool *pgxpool.Pool, runID uuid.UUID) int32 {
	t.Helper()
	var rev int32
	if err := pool.QueryRow(ctx, `SELECT contract_revision FROM runs WHERE id = $1`, runID).Scan(&rev); err != nil {
		t.Fatalf("read runs.contract_revision: %v", err)
	}
	return rev
}
