package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
)

// TestAdminJudgeRoutesCeilingLiveDB pins PRD #1184 M1's read/write split for the admin "All
// users" judge routes against the REAL h.Routes() router — the only place the RequireUser +
// RequireAdminRO chain that gates them is actually wired. A fake handler call cannot prove the
// mount, and the token-masking ceiling (cli_auth.go clears IsAdmin for any non-admin_ro token)
// is a property of the real middleware, not of a handler.
//
// For each of the three GET routes: no credential is 401, a masked uzc_ (default-scope) token
// is 403 whether its owner is an admin or not, a non-admin session cookie is 403, and only a
// uza_ (admin_ro) token reaches the handler (200).
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; ./e2e/run-store-it.sh
// provides one and sweeps this package for the LiveDB suffix.
func TestAdminJudgeRoutesCeilingLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)

	admin := cliSeedUser(t, pool, true)
	member := cliSeedUser(t, pool, false)

	adminUza := cliMintToken(t, pool, admin, clitoken.ScopeAdminRO) // keeps IsAdmin — the only 200
	adminUzc := cliMintToken(t, pool, admin, clitoken.ScopeUser)    // masked to IsAdmin=false
	memberUzc := cliMintToken(t, pool, member, clitoken.ScopeUser)
	memberJWT := cliMintJWT(t, pool, member) // a non-admin browser session

	routes := []string{
		"/api/admin/judge/recommendations",
		"/api/admin/judge/stats",
		"/api/admin/judge/category-stats",
	}
	for _, p := range routes {
		// No credential at all: RequireUser 401s before the admin gate.
		if rec := bearerReq(router, http.MethodGet, p, ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("no-credential GET %s = %d, want 401\nbody: %s", p, rec.Code, rec.Body.String())
		}
		// An admin's DEFAULT-scope token is masked to IsAdmin=false ⇒ 403 (the F1 ceiling).
		if rec := bearerReq(router, http.MethodGet, p, adminUzc); rec.Code != http.StatusForbidden {
			t.Errorf("admin uzc_ GET %s = %d, want 403 (masked to IsAdmin=false)\nbody: %s", p, rec.Code, rec.Body.String())
		}
		// A non-admin's token: 403.
		if rec := bearerReq(router, http.MethodGet, p, memberUzc); rec.Code != http.StatusForbidden {
			t.Errorf("member uzc_ GET %s = %d, want 403\nbody: %s", p, rec.Code, rec.Body.String())
		}
		// A non-admin browser session (cookie): 403 too — RequireAdminRO is on IsAdmin, not scope.
		if rec := cookieReq(t, router, http.MethodGet, p, memberJWT, ""); rec.Code != http.StatusForbidden {
			t.Errorf("member session GET %s = %d, want 403\nbody: %s", p, rec.Code, rec.Body.String())
		}
		// The admin_ro token is the one credential that reaches the handler.
		if rec := bearerReq(router, http.MethodGet, p, adminUza); rec.Code != http.StatusOK {
			t.Errorf("admin uza_ GET %s = %d, want 200 (admin_ro reads the aggregate)\nbody: %s", p, rec.Code, rec.Body.String())
		}
	}
}

// TestAdminJudgeWriteRoutesCeilingLiveDB pins PRD #1184 M2's cookie-only WRITE split for the admin
// cross-user Mark done and its Undo, against the REAL h.Routes() router — the only place the
// RequireAuth + RequireAdmin chain that gates them is wired. Unlike the reads, these are cookie-ONLY:
// a uza_ admin_ro Bearer token — which reaches the aggregate reads — 401s at RequireAuth before the
// handler exists, and a non-admin session is 403 at RequireAdmin. A valid admin session cookie is
// the one credential that writes, and it marks a DIFFERENT user's open member done (the cross-user
// fan-out) end to end.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; ./e2e/run-store-it.sh
// provides one and sweeps this package for the LiveDB suffix.
func TestAdminJudgeWriteRoutesCeilingLiveDB(t *testing.T) {
	h, router, pool := cliLiveDB(t)
	// The admin cross-user Mark-done write opens a per-coordinate advisory-lock transaction and is
	// fail-closed without a tx beginner (issue #1184 rework). Wire it as production does
	// (api/cmd/server/main.go: wsvc.SetTxBeginner(pool)) so the valid admin PUT below reaches 200.
	h.wsvc.SetTxBeginner(pool)

	admin := cliSeedUser(t, pool, true)
	member := cliSeedUser(t, pool, false)

	adminUza := cliMintToken(t, pool, admin, clitoken.ScopeAdminRO) // keeps IsAdmin, but Bearer-only
	memberJWT := cliMintJWT(t, pool, member)                        // a non-admin browser session
	adminJWT := cliMintJWT(t, pool, admin)                          // the admin browser session

	// A fresh, unique coordinate owned by the MEMBER (not the admin), so the fan-out count is
	// deterministic on the shared DB and the cross-user reach is what the 200 proves.
	target := "m2route-" + uuid.NewString()
	repoID := cliSeedOwnedRepo(t, pool, member)
	runID, reviewID := uuid.New(), uuid.New()
	cliMustExec(t, pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status)
		 VALUES ($1, $2, $3, 4242, 'r', 'd', 'completed')`, runID, member, repoID)
	cliMustExec(t, pool,
		`INSERT INTO run_reviews (id, target_run_id, user_id, verdict) VALUES ($1, $2, $3, 'issues')`,
		reviewID, runID, member)
	cliMustExec(t, pool,
		`INSERT INTO review_recommendations (review_id, category, target, rationale_md)
		 VALUES ($1, 'improve_uzi', $2, 'because')`, reviewID, target)

	body := fmt.Sprintf(`{"items":[{"category":"improve_uzi","target":%q}],"status":"done"}`, target)
	undoBody := fmt.Sprintf(`{"items":[{"category":"improve_uzi","target":%q}]}`, target)
	const p = "/api/admin/judge/recommendations/disposition"

	// ---- the auth matrix on BOTH verbs; none of these mutate (they 401/403 before the handler) --
	for _, m := range []struct {
		verb, body string
	}{{http.MethodPut, body}, {http.MethodDelete, undoBody}} {
		// No credential: RequireAuth 401s.
		if rec := bearerReqBody(router, m.verb, p, "", m.body); rec.Code != http.StatusUnauthorized {
			t.Errorf("no-credential %s %s = %d, want 401\nbody: %s", m.verb, p, rec.Code, rec.Body.String())
		}
		// A uza_ admin_ro token is a BEARER, and the write group is cookie-only: RequireAuth 401s
		// before the handler — the structural guarantee a read-only token cannot reach a write.
		if rec := bearerReqBody(router, m.verb, p, adminUza, m.body); rec.Code != http.StatusUnauthorized {
			t.Errorf("admin uza_ %s %s = %d, want 401 (cookie-only RequireAuth; a Bearer never reaches the write)\nbody: %s", m.verb, p, rec.Code, rec.Body.String())
		}
		// A non-admin browser session: RequireAdmin 403s.
		if rec := cookieReq(t, router, m.verb, p, memberJWT, m.body); rec.Code != http.StatusForbidden {
			t.Errorf("member session %s %s = %d, want 403\nbody: %s", m.verb, p, rec.Code, rec.Body.String())
		}
	}

	// ---- the valid cookie-admin PUT reaches the handler and marks the MEMBER's member done ------
	rec := cookieReq(t, router, http.MethodPut, p, adminJWT, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin session PUT = %d, want 200\nbody: %s", rec.Code, rec.Body.String())
	}
	var res apitypes.JudgeAdminDispositionResultDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode PUT result: %v; body=%s", err, rec.Body.String())
	}
	if res.Updated != 1 {
		t.Fatalf("admin PUT updated = %d, want 1 (the member's one open member, marked done cross-user)", res.Updated)
	}
	// The member's row now carries the admin provenance.
	var status string
	var setVia *string
	var setBy *uuid.UUID
	if err := pool.QueryRow(t.Context(),
		`SELECT status, set_via, set_by_user_id FROM recommendation_dispositions WHERE review_id = $1`,
		reviewID).Scan(&status, &setVia, &setBy); err != nil {
		t.Fatalf("read back the written disposition: %v", err)
	}
	if status != "done" || setVia == nil || *setVia != "admin" || setBy == nil || *setBy != admin {
		t.Fatalf("written row = status %q set_via %v set_by %v, want done/admin/%s", status, setVia, setBy, admin)
	}

	// ---- the valid cookie-admin DELETE undoes it -------------------------------------------------
	rec = cookieReq(t, router, http.MethodDelete, p, adminJWT, undoBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin session DELETE = %d, want 200\nbody: %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode DELETE result: %v; body=%s", err, rec.Body.String())
	}
	if res.Updated != 1 {
		t.Fatalf("admin DELETE updated = %d, want 1 (the admin row removed)", res.Updated)
	}
	var remaining int
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*) FROM recommendation_dispositions WHERE review_id = $1`, reviewID).Scan(&remaining); err != nil {
		t.Fatalf("count remaining: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("after the admin undo, %d disposition rows remain on the member's review, want 0", remaining)
	}
}
