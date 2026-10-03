package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/healthsvc"
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
	h, router, pool := cliLiveDB(t)

	admin := cliSeedUser(t, pool, true)
	nonAdmin := cliSeedUser(t, pool, false)
	adminUza := cliMintToken(t, pool, admin, clitoken.ScopeAdminRO) // read-only admin
	adminUzc := cliMintToken(t, pool, admin, clitoken.ScopeUser)    // admin owner, masked to non-admin

	// Inject a real registry with a real enabled row, while preserving router auth.
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	id, conn := uuid.New(), uuid.New()
	mustExecT(context.Background(), t, pool, `INSERT INTO forge_connections (id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext) VALUES ($1,$2,'gitlab','https://health.example','bot',1,$3)`, conn, admin, []byte{1})
	mustExecT(context.Background(), t, pool, `INSERT INTO repos (id,connection_id,forge_project_id,path_with_namespace,web_url,enabled) VALUES ($1,$2,1,'g/health','https://health.example/g/health',true)`, id, conn)
	registry := healthsvc.NewSyncRegistry(func() time.Time { return now })
	ids, err := h.q.ListEnabledRepoIDs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, enabledID := range ids {
		registry.Begin(enabledID)(true, forge.ErrorClassOther)
	}
	registry.Begin(id)(false, forge.ErrorClassTimeout)
	now = now.Add(10 * time.Minute)
	h.SetHealthService(healthsvc.New(healthsvc.Config{Store: h.q, Pool: pool, Settings: h.settings, Now: func() time.Time { return now }, ForgeSyncRegistry: registry, ForgeSyncInterval: time.Minute}))
	h.now = func() time.Time { return now }

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
	if len(doc.Checks) != 16 {
		t.Fatalf("health doc carries %d checks, want the 16 checks (11 M1 + controller.report, loops, forge.ciwatch from M2 + forge.sync from M3 + fleet.rundisk from PRD #1809 M6)\nbody: %s", len(doc.Checks), rec.Body.String())
	}
	if doc.Status == "" {
		t.Fatalf("health doc status is empty\nbody: %s", rec.Body.String())
	}
	foundSync := false
	for _, c := range doc.Checks {
		if c.ID == "forge.sync" {
			foundSync = true
			if c.Severity != "danger" {
				t.Fatalf("forge.sync = %+v", c)
			}
		}
	}
	if !foundSync {
		t.Fatal("missing forge.sync")
	}
	// Recovery remains cached inside the existing TTL, then appears after expiry.
	registry.Begin(id)(true, forge.ErrorClassOther)
	cached := bearerReq(router, http.MethodGet, path, adminUza)
	if err := json.Unmarshal(cached.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	for _, c := range doc.Checks {
		if c.ID == "forge.sync" && c.Severity != "danger" {
			t.Fatal("health cache changed within TTL")
		}
	}
	now = now.Add(healthCacheTTL + time.Second)
	recovered := bearerReq(router, http.MethodGet, path, adminUza)
	if recovered.Code != http.StatusOK {
		t.Fatal(recovered.Body.String())
	}
	if err := json.Unmarshal(recovered.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	for _, c := range doc.Checks {
		if c.ID == "forge.sync" && c.Severity != "ok" {
			t.Fatalf("recovery: %+v", c)
		}
	}

}
