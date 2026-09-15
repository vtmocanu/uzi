package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/clitoken"
)

// The live-DB Bearer-accept proof for PRD #1247 M4 (the MANDATORY router-level auth test,
// D4's cookie-only-route trap): `uzi run set-token` POSTs to /api/runs/{id}/credential,
// which is mounted in the RequireUser /runs group so a uzc_ CLI Bearer reaches it — NOT the
// cookie-only RequireAuth group. A fake-client unit test CANNOT catch a mis-mount; only
// driving the REAL h.Routes() router through the mounted middleware chain proves which group
// handler.go wired the path into. It also exercises the full HTTP-status mapping
// (200/404/409/422/400) end to end through the real service + schema.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; ./e2e/run-store-it.sh
// provides one and sweeps this package for the LiveDB suffix.
func TestSetRunCredentialCLIBearerAcceptedLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	owner := cliSeedUser(t, pool, false)
	uzc := cliMintToken(t, pool, owner, clitoken.ScopeUser)
	repoID := cliSeedOwnedRepo(t, pool, owner)
	tokenID := seedSetTokenAnthropic(t, pool, owner, "set-token-a-"+uuid.NewString())

	// (a) A uzc_ Bearer POST reaches the handler and switches a queued run — 200, NOT the
	// 401 a cookie-only mount would return before the handler ever runs. This is the mount
	// proof: the whole test's point.
	runID := seedSetTokenRun(t, pool, owner, repoID, "queued", "claude", 7401)
	body := fmt.Sprintf(`{"mode":"pinned","secret_id":%q}`, tokenID)
	rec := bearerReqBody(router, http.MethodPost, "/api/runs/"+runID.String()+"/credential", uzc, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("Bearer POST credential = %d, want 200 (RequireUser mount, NOT the 401 a cookie-only mount would return)\nbody: %s", rec.Code, rec.Body.String())
	}
	var envelope struct {
		Run     json.RawMessage `json:"run"`
		Warning string          `json:"warning"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode {run, warning}: %v\nbody: %s", err, rec.Body.String())
	}
	if len(envelope.Run) == 0 {
		t.Fatalf("200 response carried no run object: %s", rec.Body.String())
	}

	// (b) No credential at all → 401: the mount is Bearer-OR-cookie, not public.
	rec = bearerReqBody(router, http.MethodPost, "/api/runs/"+runID.String()+"/credential", "", body)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no-credential POST credential = %d, want 401\nbody: %s", rec.Code, rec.Body.String())
	}

	// (c) 404: a run owned by ANOTHER user — the owner-scoped read leaks no existence oracle.
	other := cliSeedUser(t, pool, false)
	otherRepo := cliSeedOwnedRepo(t, pool, other)
	foreignRun := seedSetTokenRun(t, pool, other, otherRepo, "queued", "claude", 7402)
	rec = bearerReqBody(router, http.MethodPost, "/api/runs/"+foreignRun.String()+"/credential", uzc, body)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign run POST = %d, want 404\nbody: %s", rec.Code, rec.Body.String())
	}

	// (d) 409: a held (running) run — the held-state switch protocol is M5.
	runningRun := seedSetTokenRun(t, pool, owner, repoID, "running", "claude", 7403)
	rec = bearerReqBody(router, http.MethodPost, "/api/runs/"+runningRun.String()+"/credential", uzc, body)
	if rec.Code != http.StatusConflict {
		t.Fatalf("running run POST = %d, want 409 (held state)\nbody: %s", rec.Code, rec.Body.String())
	}

	// (e) 409: a terminal (completed) run.
	doneRun := seedSetTokenRun(t, pool, owner, repoID, "completed", "claude", 7404)
	rec = bearerReqBody(router, http.MethodPost, "/api/runs/"+doneRun.String()+"/credential", uzc, body)
	if rec.Code != http.StatusConflict {
		t.Fatalf("completed run POST = %d, want 409 (terminal)\nbody: %s", rec.Code, rec.Body.String())
	}

	// (f) 422: a codex-harness run — a credential override is Anthropic-only (D9). Checked
	// before the status dispatch, so it fires even on an otherwise-switchable queued run.
	codexRun := seedSetTokenRun(t, pool, owner, repoID, "queued", "codex", 7405)
	rec = bearerReqBody(router, http.MethodPost, "/api/runs/"+codexRun.String()+"/credential", uzc, body)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("codex run POST = %d, want 422\nbody: %s", rec.Code, rec.Body.String())
	}

	// (g) 400: a mode outside the closed set.
	rec = bearerReqBody(router, http.MethodPost, "/api/runs/"+runID.String()+"/credential", uzc, `{"mode":"bogus"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad-mode POST = %d, want 400\nbody: %s", rec.Code, rec.Body.String())
	}

	// (h) 400: a malformed secret_id uuid, rejected by the handler before the service.
	rec = bearerReqBody(router, http.MethodPost, "/api/runs/"+runID.String()+"/credential", uzc, `{"mode":"pinned","secret_id":"not-a-uuid"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed secret_id POST = %d, want 400\nbody: %s", rec.Code, rec.Body.String())
	}
}

// seedSetTokenAnthropic inserts one anthropic_token user_secret owned by userID and returns
// its id (metadata only; the set-token validator reads metadata, not decryptable material).
func seedSetTokenAnthropic(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID, label string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	cliMustExec(t, pool,
		`INSERT INTO user_secrets (id, user_id, kind, label, ciphertext, sealed_with)
		 VALUES ($1, $2, 'anthropic_token', $3, $4, 'master')`,
		id, userID, label, []byte("x"))
	return id
}

// seedSetTokenRun inserts one run owned by userID in the given status and harness.
func seedSetTokenRun(t *testing.T, pool *pgxpool.Pool, userID, repoID uuid.UUID, status, harness string, issueIID int64) uuid.UUID {
	t.Helper()
	id := uuid.New()
	cliMustExec(t, pool,
		`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, harness)
		 VALUES ($1, $2, $3, 'issue', $4, 't', 'd', $5, $6)`,
		id, userID, repoID, issueIID, status, harness)
	return id
}
