package handler

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/clitoken"
)

// The live-DB half of PRD #357 M1: the explicit per-repo remove
// (DELETE /api/repos/{id}). It ships as a live-DB suite, driving the REAL
// h.Routes() router, because the cascade is the point (a fake store cannot show
// runs/run_messages disappearing via FK) and because the auth MOUNT is load-bearing:
// the route sits in the RequireUser group so the CLI's Bearer token is accepted — a
// cookie-only RequireAuth mount would 401 it (issue #428 regression), and only the
// real router proves which group handler.go wired the path into.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres;
// ./e2e/run-store-it.sh provides one and sweeps this package for the LiveDB suffix.

// rmSeedConn inserts a forge connection owned by userID and returns its id. The
// connection is the tenancy anchor: DeleteRepoForUser is scoped through it.
func rmSeedConn(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID) uuid.UUID {
	t.Helper()
	connID := uuid.New()
	cliMustExec(t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	return connID
}

// rmSeedRepo inserts a repo with a distinct forge project id under connID and
// returns its uuid.
func rmSeedRepo(t *testing.T, pool *pgxpool.Pool, connID uuid.UUID, projectID int64, enabled bool) uuid.UUID {
	t.Helper()
	repoID := uuid.New()
	cliMustExec(t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, $3, $4, $5, 'main', $6)`,
		repoID, connID, projectID, fmt.Sprintf("g/r-%d", projectID), fmt.Sprintf("https://forge.e2e/g/r-%d", projectID), enabled)
	return repoID
}

// rmSeedRun inserts a run in the given status for repoID and returns its id.
func rmSeedRun(t *testing.T, pool *pgxpool.Pool, userID, repoID uuid.UUID, status string) uuid.UUID {
	t.Helper()
	runID := uuid.New()
	cliMustExec(t, pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind)
		 VALUES ($1, $2, $3, 7, 'Do X', 'desc', $4, 'issue')`, runID, userID, repoID, status)
	return runID
}

func rmRepoExists(t *testing.T, pool *pgxpool.Pool, repoID uuid.UUID) bool {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM repos WHERE id = $1`, repoID).Scan(&n); err != nil {
		t.Fatalf("count repo: %v", err)
	}
	return n > 0
}

func rmRunCount(t *testing.T, pool *pgxpool.Pool, repoID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM runs WHERE repo_id = $1`, repoID).Scan(&n); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	return n
}

func rmRunMessageCount(t *testing.T, pool *pgxpool.Pool, runID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM run_messages WHERE run_id = $1`, runID).Scan(&n); err != nil {
		t.Fatalf("count run_messages: %v", err)
	}
	return n
}

// (a) The owner removes a DISABLED repo via the real router → 204, and the row is
// gone. Positive control: the row exists before the delete, so the "gone" assertion
// cannot pass vacuously.
func TestDeleteRepoDisabledSucceedsLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	owner := cliSeedUser(t, pool, false)
	jwt := cliMintJWT(t, pool, owner)
	connID := rmSeedConn(t, pool, owner)
	repoID := rmSeedRepo(t, pool, connID, 201, false)

	if !rmRepoExists(t, pool, repoID) {
		t.Fatalf("positive control: the repo must exist before deletion")
	}
	rec := cookieReq(t, router, http.MethodDelete, "/api/repos/"+repoID.String(), jwt, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE disabled repo = %d, want 204\nbody: %s", rec.Code, rec.Body.String())
	}
	if rmRepoExists(t, pool, repoID) {
		t.Errorf("the repos row must be gone after a 204")
	}
}

// (b) Removing an ENABLED repo → 409, and the row survives (D2: disable first).
func TestDeleteRepoEnabledIs409LiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	owner := cliSeedUser(t, pool, false)
	jwt := cliMintJWT(t, pool, owner)
	connID := rmSeedConn(t, pool, owner)
	repoID := rmSeedRepo(t, pool, connID, 202, true)

	rec := cookieReq(t, router, http.MethodDelete, "/api/repos/"+repoID.String(), jwt, "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("DELETE enabled repo = %d, want 409\nbody: %s", rec.Code, rec.Body.String())
	}
	if !rmRepoExists(t, pool, repoID) {
		t.Errorf("an enabled repo refused with 409 must survive")
	}
}

// (c) A foreign/unknown id → 404 (GetRepoForUser scoping).
func TestDeleteRepoForeignIs404LiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	owner := cliSeedUser(t, pool, false)
	other := cliSeedUser(t, pool, false)
	jwt := cliMintJWT(t, pool, owner)

	// A wholly unknown id.
	if rec := cookieReq(t, router, http.MethodDelete, "/api/repos/"+uuid.New().String(), jwt, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("DELETE unknown repo = %d, want 404\nbody: %s", rec.Code, rec.Body.String())
	}
	// A repo that exists but belongs to ANOTHER user is a 404 for the owner, and it
	// must survive — the 404 is the scoping, not a missing row.
	otherConn := rmSeedConn(t, pool, other)
	foreign := rmSeedRepo(t, pool, otherConn, 203, false)
	if rec := cookieReq(t, router, http.MethodDelete, "/api/repos/"+foreign.String(), jwt, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("DELETE foreign repo = %d, want 404\nbody: %s", rec.Code, rec.Body.String())
	}
	if !rmRepoExists(t, pool, foreign) {
		t.Errorf("a foreign repo (404) must not be deleted by a non-owner")
	}
}

// (d) A DISABLED repo with a NON-TERMINAL run → 409, and the row survives (D7:
// "disabled" is not "quiescent"). Table-driven over EACH non-terminal status rather
// than a single "running" fixture, because the guard is the terminal-complement set
// (CountActiveRunsForRepo, forge.sql) and limit_wait — a parked run that resumes on
// its own (PRD #35, runlifecycle groups it with the other In Progress statuses) —
// is the specific state a positive-enumeration query silently omitted, letting a
// parked run get cascade-deleted. Each status seeds its own disabled repo; positive
// control per status: the row exists before the delete, so "survives" cannot pass
// vacuously. Then a sibling disabled repo with NO active run deletes (204), proving
// the 409 is the run guard, not the disabled state.
func TestDeleteRepoWithActiveRunIs409LiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	owner := cliSeedUser(t, pool, false)
	jwt := cliMintJWT(t, pool, owner)
	connID := rmSeedConn(t, pool, owner)

	nonTerminal := []string{"limit_wait", "awaiting_input", "awaiting_approval", "queued", "running"}
	for i, status := range nonTerminal {
		repoID := rmSeedRepo(t, pool, connID, int64(220+i), false)
		rmSeedRun(t, pool, owner, repoID, status)

		if !rmRepoExists(t, pool, repoID) {
			t.Fatalf("positive control (%s): the repo must exist before the delete attempt", status)
		}
		rec := cookieReq(t, router, http.MethodDelete, "/api/repos/"+repoID.String(), jwt, "")
		if rec.Code != http.StatusConflict {
			t.Fatalf("DELETE disabled repo with a %s run = %d, want 409\nbody: %s", status, rec.Code, rec.Body.String())
		}
		if !rmRepoExists(t, pool, repoID) {
			t.Errorf("a repo with a %s (non-terminal) run refused with 409 must survive", status)
		}
	}

	// Positive control: a sibling disabled repo with NO active run deletes cleanly,
	// so the 409s above are specifically the active-run guard.
	clean := rmSeedRepo(t, pool, connID, 205, false)
	if rec := cookieReq(t, router, http.MethodDelete, "/api/repos/"+clean.String(), jwt, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE disabled repo with no active run = %d, want 204\nbody: %s", rec.Code, rec.Body.String())
	}
}

// (e) Cascade proof: a disabled repo with a TERMINAL run that has a run_messages
// child → after 204 both the run and its run_messages are gone, while a SIBLING repo
// on the same connection (and its run) is untouched. Positive controls: the run and
// its message exist before the delete.
func TestDeleteRepoCascadesAndSpareSiblingLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	owner := cliSeedUser(t, pool, false)
	jwt := cliMintJWT(t, pool, owner)
	connID := rmSeedConn(t, pool, owner)

	target := rmSeedRepo(t, pool, connID, 206, false)
	targetRun := rmSeedRun(t, pool, owner, target, "completed")
	cliMustExec(t, pool,
		`INSERT INTO run_messages (run_id, seq, kind, payload) VALUES ($1, 1, 'text', $2)`,
		targetRun, []byte(`{"text":"hi"}`))

	// A sibling repo on the SAME connection, with its own terminal run — must survive.
	sibling := rmSeedRepo(t, pool, connID, 207, false)
	siblingRun := rmSeedRun(t, pool, owner, sibling, "completed")

	// Positive controls.
	if rmRunCount(t, pool, target) != 1 {
		t.Fatalf("positive control: target must have its run before deletion")
	}
	if rmRunMessageCount(t, pool, targetRun) != 1 {
		t.Fatalf("positive control: target run must have its message before deletion")
	}

	rec := cookieReq(t, router, http.MethodDelete, "/api/repos/"+target.String(), jwt, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE disabled repo (cascade) = %d, want 204\nbody: %s", rec.Code, rec.Body.String())
	}

	if rmRepoExists(t, pool, target) {
		t.Errorf("the removed repo row must be gone")
	}
	if rmRunCount(t, pool, target) != 0 {
		t.Errorf("the removed repo's runs must cascade away")
	}
	if rmRunMessageCount(t, pool, targetRun) != 0 {
		t.Errorf("the removed run's run_messages must cascade away (transitively via runs)")
	}

	// The sibling and its subtree are untouched.
	if !rmRepoExists(t, pool, sibling) {
		t.Errorf("a sibling repo on the same connection must be untouched")
	}
	if rmRunCount(t, pool, sibling) != 1 {
		t.Errorf("the sibling repo's runs must survive (count=%d, want 1)", rmRunCount(t, pool, sibling))
	}
	_ = siblingRun
}

// (f) Differential auth (the mis-mount guard, issue #428). A valid uzc_ Bearer with
// NO cookie hitting DELETE /api/repos/{id} gets the REAL 204 (disabled) / 409
// (enabled) / 404 (foreign) — proving the route is on the RequireUser mount, since a
// cookie-only RequireAuth mount would 401 the Bearer before the handler ran. And a
// no-credential request (no cookie, no Bearer) gets 401.
func TestDeleteRepoBearerMountAndNoCredLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	owner := cliSeedUser(t, pool, false)
	uzc := cliMintToken(t, pool, owner, clitoken.ScopeUser)
	connID := rmSeedConn(t, pool, owner)

	disabled := rmSeedRepo(t, pool, connID, 208, false)
	enabled := rmSeedRepo(t, pool, connID, 209, true)

	// Bearer reaches the handler (RequireUser mount) and gets the real status codes.
	if rec := bearerReq(router, http.MethodDelete, "/api/repos/"+enabled.String(), uzc); rec.Code != http.StatusConflict {
		t.Fatalf("Bearer DELETE enabled repo = %d, want 409 (proves the Bearer reached the handler)\nbody: %s", rec.Code, rec.Body.String())
	}
	if rec := bearerReq(router, http.MethodDelete, "/api/repos/"+uuid.New().String(), uzc); rec.Code != http.StatusNotFound {
		t.Fatalf("Bearer DELETE unknown repo = %d, want 404\nbody: %s", rec.Code, rec.Body.String())
	}
	// The 204 last so the row it removes does not affect the assertions above.
	if rec := bearerReq(router, http.MethodDelete, "/api/repos/"+disabled.String(), uzc); rec.Code != http.StatusNoContent {
		t.Fatalf("Bearer DELETE disabled repo = %d, want 204 (a cookie-only mount would 401 this)\nbody: %s", rec.Code, rec.Body.String())
	}
	if rmRepoExists(t, pool, disabled) {
		t.Errorf("the Bearer-removed repo row must be gone")
	}

	// No credential at all → 401 (the mount still requires auth; the move is
	// cookie→also-bearer, not authenticated→public).
	stillThere := rmSeedRepo(t, pool, connID, 210, false)
	if rec := bearerReq(router, http.MethodDelete, "/api/repos/"+stillThere.String(), ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no-credential DELETE = %d, want 401\nbody: %s", rec.Code, rec.Body.String())
	}
	if !rmRepoExists(t, pool, stillThere) {
		t.Errorf("an unauthenticated DELETE must not remove the repo")
	}
}
