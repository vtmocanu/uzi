package handler

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/clitoken"
)

// TestAdminHealthAuthLiveDB proves the GET /api/admin/health mount (PRD #1484 M1) enforces
// the RequireUser + RequireAdminRO gate through the REAL chi router — the property a
// fake-client test cannot show (it bypasses the middleware chain). The endpoint carries
// owner and worker names, so a uzc_ token and a non-admin cookie must both be refused, and
// only a uza_ admin token gets 200. It also confirms the 200 body is the full closed
// registry, so the auth pass is not masking an error envelope.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; ./e2e/run-store-it.sh
// provides one and sweeps this package for the LiveDB suffix.
func TestAdminHealthAuthLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)

	admin := cliSeedUser(t, pool, true)
	nonAdmin := cliSeedUser(t, pool, false)
	adminUza := cliMintToken(t, pool, admin, clitoken.ScopeAdminRO) // read-only admin
	adminUzc := cliMintToken(t, pool, admin, clitoken.ScopeUser)    // admin owner, masked to non-admin

	const path = "/api/admin/health"

	// Anonymous (no credential) → 401.
	if rec := bearerReq(router, http.MethodGet, path, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous GET %s = %d, want 401\nbody: %s", path, rec.Code, rec.Body.String())
	}
	// A uzc_ CLI token is masked to non-admin → 403 (the owner/worker names must not reach it).
	if rec := bearerReq(router, http.MethodGet, path, adminUzc); rec.Code != http.StatusForbidden {
		t.Errorf("uzc_ GET %s = %d, want 403 (masked non-admin)\nbody: %s", path, rec.Code, rec.Body.String())
	}
	// A non-admin session cookie → 403.
	if rec := cookieReq(t, router, http.MethodGet, path, cliMintJWT(t, pool, nonAdmin), ""); rec.Code != http.StatusForbidden {
		t.Errorf("non-admin cookie GET %s = %d, want 403\nbody: %s", path, rec.Code, rec.Body.String())
	}
	// A uza_ admin token → 200 with the full registry.
	rec := bearerReq(router, http.MethodGet, path, adminUza)
	if rec.Code != http.StatusOK {
		t.Fatalf("uza_ GET %s = %d, want 200\nbody: %s", path, rec.Code, rec.Body.String())
	}
	var doc struct {
		Status string `json:"status"`
		Checks []struct {
			ID       string `json:"id"`
			Severity string `json:"severity"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode health doc: %v (body %s)", err, rec.Body.String())
	}
	if len(doc.Checks) != 11 {
		t.Fatalf("health doc carries %d checks, want the 11 M1 checks\nbody: %s", len(doc.Checks), rec.Body.String())
	}
	if doc.Status == "" {
		t.Fatalf("health doc status is empty\nbody: %s", rec.Body.String())
	}
}
