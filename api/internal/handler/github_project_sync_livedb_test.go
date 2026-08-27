package handler

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// The live-DB half of issue #534 M1: the four per-repo GitHub-project-sync routes
// were relocated out of /admin (admin-only) into the per-repo RequireAuth group with
// owner-or-admin authorization, guarded by a GetRepoForUser preflight inside each
// handler (D4). These tests drive the REAL h.Routes() router because the AUTHORIZATION
// is the point: the preflight's existence-hiding 404 for a non-owner, and the admin's
// skip of it, are only observable through the real mounted middleware chain + handler.
//
// The discriminator, and why it needs no forge/service seeding: cliLiveDB builds the
// Handler with projectSync == nil. So a request that PASSES the preflight falls
// through to each handler's `h.projectSync == nil` guard and returns 500 ("service not
// wired"); a request BLOCKED by the preflight returns 404 ("repo not found") before
// ever reaching that guard. The two codes therefore cleanly separate "preflight
// passed" (500) from "preflight blocked" (404) — the exact property under test —
// without wiring the service or seeding a github_project_links row. (The service's own
// no-link 404 never occurs here, since projectSync is nil and 500 fires first, so the
// 404 seen below is unambiguously the owner preflight.)
//
// The mutating link routes exercised here (DELETE sync, POST provision) sit in the
// per-repo RequireAuth (cookie-only) group beside SetRepoEnabled / PatchRepo, so they
// use the cookie+CSRF path (cookieReq). PRD #576 M7 moved the status READ (GET) and
// resync UP to the RequireUser group so the CLI's uzc_ Bearer reaches them; a cookie
// works on that group too, so the GET-status cases below still drive it via cookieReq.
// The Bearer-accept proof for the moved routes lives in the *CLIBearer* LiveDB test.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres;
// ./e2e/run-store-it.sh provides one and sweeps this package for the LiveDB suffix.

// gpsStatusPath / gpsSyncPath build the relocated route URLs for a repo id.
func gpsStatusPath(repoID uuid.UUID) string {
	return "/api/repos/" + repoID.String() + "/github-project-sync"
}

// gpsVisibilityPath / gpsCollaboratorsPath build the PRD #557 board-access route URLs.
func gpsVisibilityPath(repoID uuid.UUID) string    { return gpsStatusPath(repoID) + "/visibility" }
func gpsCollaboratorsPath(repoID uuid.UUID) string { return gpsStatusPath(repoID) + "/collaborators" }

// (a) The repo OWNER (a non-admin member) reaches the preflight-guarded handler for
// their OWN repo: the preflight PASSES, so the request is NOT 404'd by it — it falls
// through to the service-not-wired 500 (projectSync is nil in this harness). A foreign
// repo, by contrast, is 404'd at the preflight. Asserting own-repo != preflight-404
// (here == 500) while foreign == 404 isolates the preflight for both a READ (GET) and
// a WRITE (DELETE).
func TestGithubProjectSyncOwnerPassesPreflightLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	owner := cliSeedUser(t, pool, false)
	jwt := cliMintJWT(t, pool, owner)
	connID := rmSeedConn(t, pool, owner)
	ownRepo := rmSeedRepo(t, pool, connID, 534001, false)

	// GET on the owner's own repo: past the preflight ⇒ the service-not-wired 500, NOT
	// the preflight's 404. If this were 404 the owner would be wrongly existence-hidden
	// from their own repo.
	if rec := cookieReq(t, router, http.MethodGet, gpsStatusPath(ownRepo), jwt, ""); rec.Code != http.StatusInternalServerError {
		t.Fatalf("owner GET own repo status = %d, want 500 (preflight passes → service-not-wired), NOT the preflight 404\nbody: %s", rec.Code, rec.Body.String())
	}
	// DELETE on the owner's own repo: same, past the preflight ⇒ 500 not 404.
	if rec := cookieReq(t, router, http.MethodDelete, gpsStatusPath(ownRepo), jwt, ""); rec.Code != http.StatusInternalServerError {
		t.Fatalf("owner DELETE own repo sync = %d, want 500 (preflight passes → service-not-wired), NOT the preflight 404\nbody: %s", rec.Code, rec.Body.String())
	}
}

// (b) A non-owner member acting on ANOTHER user's repo is existence-hidden: 404 for
// both a READ (GET) and a WRITE (DELETE, POST provision), and a wholly-unknown id is
// 404 too. The foreign repo must survive (the 404 is the scoping, not a missing row).
func TestGithubProjectSyncForeignIs404LiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	member := cliSeedUser(t, pool, false)
	other := cliSeedUser(t, pool, false)
	jwt := cliMintJWT(t, pool, member)

	// A wholly-unknown id ⇒ 404 at the preflight.
	if rec := cookieReq(t, router, http.MethodGet, gpsStatusPath(uuid.New()), jwt, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("member GET unknown repo status = %d, want 404 (preflight existence-hiding)\nbody: %s", rec.Code, rec.Body.String())
	}

	// A repo that exists but belongs to ANOTHER user ⇒ 404 for the member, and it must
	// survive — the 404 is the owner scoping, not a missing row.
	otherConn := rmSeedConn(t, pool, other)
	foreign := rmSeedRepo(t, pool, otherConn, 534002, false)

	// READ.
	if rec := cookieReq(t, router, http.MethodGet, gpsStatusPath(foreign), jwt, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("member GET foreign repo status = %d, want 404 (existence-hiding)\nbody: %s", rec.Code, rec.Body.String())
	}
	// WRITE (DELETE).
	if rec := cookieReq(t, router, http.MethodDelete, gpsStatusPath(foreign), jwt, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("member DELETE foreign repo sync = %d, want 404 (existence-hiding)\nbody: %s", rec.Code, rec.Body.String())
	}
	// WRITE (POST provision): the preflight runs before body decode, so an empty body
	// still 404s at the preflight for a non-owner.
	if rec := cookieReq(t, router, http.MethodPost, gpsStatusPath(foreign)+"/provision", jwt, "{}"); rec.Code != http.StatusNotFound {
		t.Fatalf("member POST provision foreign repo = %d, want 404 (existence-hiding)\nbody: %s", rec.Code, rec.Body.String())
	}

	if !rmRepoExists(t, pool, foreign) {
		t.Errorf("a foreign repo (404) must not be touched by a non-owner")
	}
}

// (c) An ADMIN reaches any repo: the preflight is SKIPPED for an admin, so an admin
// request on a FOREIGN repo (one the admin does not own) is NOT 404'd — it falls
// through to the service-not-wired 500, on the SAME foreign repo where a non-admin
// member gets the preflight 404. That differential (non-admin 404 vs admin 500 on the
// identical repo) is the discriminator that the admin bypass is real, not that the
// repo happens to resolve. Covers a READ and a WRITE.
func TestGithubProjectSyncAdminSkipsPreflightLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	admin := cliSeedUser(t, pool, true)
	owner := cliSeedUser(t, pool, false)
	stranger := cliSeedUser(t, pool, false)
	adminJWT := cliMintJWT(t, pool, admin)
	strangerJWT := cliMintJWT(t, pool, stranger)

	// A repo owned by `owner` — foreign to BOTH the admin and the stranger (neither
	// owns this connection).
	ownerConn := rmSeedConn(t, pool, owner)
	foreign := rmSeedRepo(t, pool, ownerConn, 534003, false)

	// The non-admin control: a non-owner (the stranger) is 404'd at the preflight on
	// this exact repo, so the admin's non-404 below is specifically the admin skip and
	// not that the repo happens to resolve.
	if rec := cookieReq(t, router, http.MethodGet, gpsStatusPath(foreign), strangerJWT, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("non-admin stranger GET foreign repo = %d, want 404 (preflight blocks a non-owner)\nbody: %s", rec.Code, rec.Body.String())
	}
	if rec := cookieReq(t, router, http.MethodDelete, gpsStatusPath(foreign), strangerJWT, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("non-admin stranger DELETE foreign repo = %d, want 404 (preflight blocks a non-owner)\nbody: %s", rec.Code, rec.Body.String())
	}

	// The admin on the SAME foreign repo: preflight skipped ⇒ past it to the
	// service-not-wired 500, NOT the preflight 404 — for both a READ and a WRITE.
	if rec := cookieReq(t, router, http.MethodGet, gpsStatusPath(foreign), adminJWT, ""); rec.Code != http.StatusInternalServerError {
		t.Fatalf("admin GET foreign repo = %d, want 500 (admin skips the preflight → service-not-wired), NOT the preflight 404\nbody: %s", rec.Code, rec.Body.String())
	}
	if rec := cookieReq(t, router, http.MethodDelete, gpsStatusPath(foreign), adminJWT, ""); rec.Code != http.StatusInternalServerError {
		t.Fatalf("admin DELETE foreign repo = %d, want 500 (admin skips the preflight), NOT the preflight 404\nbody: %s", rec.Code, rec.Body.String())
	}
}

// PRD #557 M3 — the SAME authorization boundary, extended to the four new board-access
// routes (GET/PUT visibility, POST/DELETE collaborators). Same discriminator as above:
// projectSync == nil, so a request PAST the preflight hits the nil-guard 500 and a
// BLOCKED request is 404. These prove the 404-vs-500 boundary only; behavior (409/422,
// the actual forge call) is the httptest tier's job.

// TestGithubProjectVisibilityAuthLiveDB: owner/admin reach the visibility routes past
// the preflight (500), a foreign non-owner is existence-hidden (404), and the foreign
// repo survives — for both the GET read and the PUT write.
func TestGithubProjectVisibilityAuthLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	admin := cliSeedUser(t, pool, true)
	owner := cliSeedUser(t, pool, false)
	member := cliSeedUser(t, pool, false)
	adminJWT := cliMintJWT(t, pool, admin)
	ownerJWT := cliMintJWT(t, pool, owner)
	memberJWT := cliMintJWT(t, pool, member)

	ownerConn := rmSeedConn(t, pool, owner)
	ownRepo := rmSeedRepo(t, pool, ownerConn, 557101, false)

	// Owner passes the preflight on their own repo ⇒ nil-guard 500, NOT the preflight 404.
	if rec := cookieReq(t, router, http.MethodGet, gpsVisibilityPath(ownRepo), ownerJWT, ""); rec.Code != http.StatusInternalServerError {
		t.Fatalf("owner GET visibility own repo = %d, want 500 (preflight passes)\nbody: %s", rec.Code, rec.Body.String())
	}
	if rec := cookieReq(t, router, http.MethodPut, gpsVisibilityPath(ownRepo), ownerJWT, `{"public":true}`); rec.Code != http.StatusInternalServerError {
		t.Fatalf("owner PUT visibility own repo = %d, want 500 (preflight passes)\nbody: %s", rec.Code, rec.Body.String())
	}

	// A non-owner member is existence-hidden ⇒ 404 for both the READ and the WRITE. The
	// preflight runs before body decode, so a `{}` body still 404s for a non-owner.
	if rec := cookieReq(t, router, http.MethodGet, gpsVisibilityPath(ownRepo), memberJWT, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("member GET visibility foreign repo = %d, want 404 (existence-hiding)\nbody: %s", rec.Code, rec.Body.String())
	}
	if rec := cookieReq(t, router, http.MethodPut, gpsVisibilityPath(ownRepo), memberJWT, `{}`); rec.Code != http.StatusNotFound {
		t.Fatalf("member PUT visibility foreign repo = %d, want 404 (existence-hiding)\nbody: %s", rec.Code, rec.Body.String())
	}

	// The admin skips the preflight on the SAME repo (foreign to the admin) ⇒ 500, the
	// differential that proves the admin bypass is real.
	if rec := cookieReq(t, router, http.MethodGet, gpsVisibilityPath(ownRepo), adminJWT, ""); rec.Code != http.StatusInternalServerError {
		t.Fatalf("admin GET visibility foreign repo = %d, want 500 (admin skips preflight)\nbody: %s", rec.Code, rec.Body.String())
	}
	if rec := cookieReq(t, router, http.MethodPut, gpsVisibilityPath(ownRepo), adminJWT, `{"public":false}`); rec.Code != http.StatusInternalServerError {
		t.Fatalf("admin PUT visibility foreign repo = %d, want 500 (admin skips preflight)\nbody: %s", rec.Code, rec.Body.String())
	}

	if !rmRepoExists(t, pool, ownRepo) {
		t.Errorf("the repo must survive the authorization checks")
	}
}

// TestGithubProjectCollaboratorsAuthLiveDB: owner/admin reach the collaborator routes
// past the preflight (500), a foreign non-owner is existence-hidden (404), and the
// foreign repo survives — for both the POST share and the DELETE unshare.
func TestGithubProjectCollaboratorsAuthLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	admin := cliSeedUser(t, pool, true)
	owner := cliSeedUser(t, pool, false)
	member := cliSeedUser(t, pool, false)
	adminJWT := cliMintJWT(t, pool, admin)
	ownerJWT := cliMintJWT(t, pool, owner)
	memberJWT := cliMintJWT(t, pool, member)

	ownerConn := rmSeedConn(t, pool, owner)
	ownRepo := rmSeedRepo(t, pool, ownerConn, 557102, false)

	// Owner passes the preflight ⇒ nil-guard 500. A `{"username":"x"}` body reaches the
	// nil-guard (ShareWithUser/Unshare never run because projectSync is nil).
	if rec := cookieReq(t, router, http.MethodPost, gpsCollaboratorsPath(ownRepo), ownerJWT, `{"username":"x"}`); rec.Code != http.StatusInternalServerError {
		t.Fatalf("owner POST collaborators own repo = %d, want 500 (preflight passes)\nbody: %s", rec.Code, rec.Body.String())
	}
	if rec := cookieReq(t, router, http.MethodDelete, gpsCollaboratorsPath(ownRepo), ownerJWT, `{"username":"x"}`); rec.Code != http.StatusInternalServerError {
		t.Fatalf("owner DELETE collaborators own repo = %d, want 500 (preflight passes)\nbody: %s", rec.Code, rec.Body.String())
	}

	// A non-owner member is existence-hidden ⇒ 404. The preflight runs before body
	// decode, so an empty body still 404s for a non-owner.
	if rec := cookieReq(t, router, http.MethodPost, gpsCollaboratorsPath(ownRepo), memberJWT, `{}`); rec.Code != http.StatusNotFound {
		t.Fatalf("member POST collaborators foreign repo = %d, want 404 (existence-hiding)\nbody: %s", rec.Code, rec.Body.String())
	}
	if rec := cookieReq(t, router, http.MethodDelete, gpsCollaboratorsPath(ownRepo), memberJWT, `{}`); rec.Code != http.StatusNotFound {
		t.Fatalf("member DELETE collaborators foreign repo = %d, want 404 (existence-hiding)\nbody: %s", rec.Code, rec.Body.String())
	}

	// The admin skips the preflight on the SAME repo ⇒ 500 for both writes.
	if rec := cookieReq(t, router, http.MethodPost, gpsCollaboratorsPath(ownRepo), adminJWT, `{"username":"x"}`); rec.Code != http.StatusInternalServerError {
		t.Fatalf("admin POST collaborators foreign repo = %d, want 500 (admin skips preflight)\nbody: %s", rec.Code, rec.Body.String())
	}
	if rec := cookieReq(t, router, http.MethodDelete, gpsCollaboratorsPath(ownRepo), adminJWT, `{"username":"x"}`); rec.Code != http.StatusInternalServerError {
		t.Fatalf("admin DELETE collaborators foreign repo = %d, want 500 (admin skips preflight)\nbody: %s", rec.Code, rec.Body.String())
	}

	if !rmRepoExists(t, pool, ownRepo) {
		t.Errorf("the repo must survive the authorization checks")
	}
}
