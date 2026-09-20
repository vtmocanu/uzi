package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/clitoken"
)

// M2-B handler live-DB coverage (PRD #1484): the per-admin snooze endpoint, the per-caller
// episode_id/snoozed_until on GetAdminHealth, and the folded-in end-to-end singleton path
// (the M2-A reviewers' gap). Router-level or real-handler drives against a live DB — a
// fake-client test cannot show the mount or the cross-caller isolation.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; ./e2e/run-store-it.sh
// provides one and sweeps this package for the LiveDB suffix.

// healthClearEpisodes deletes every health episode (cascading to snoozes/notices) so a test
// starts from a known "no open episode" baseline — the LiveDB runner shares one DB and the
// partial unique index allows only one open episode at a time.
func healthClearEpisodes(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	cliMustExec(t, pool, `DELETE FROM health_episodes`)
}

// healthOpenEpisode inserts one open episode and returns its id.
func healthOpenEpisode(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO health_episodes (opened_at) VALUES (now()) RETURNING id`).Scan(&id); err != nil {
		t.Fatalf("open episode: %v", err)
	}
	return id
}

// TestAdminHealthSnoozeAuthLiveDB proves the POST /api/admin/health/snooze mount enforces
// the cookie-only RequireAuth + RequireAdmin write gate and the 409-when-no-episode contract
// (PRD #1484 D2), and that a successful snooze writes the per-(episode, caller) row.
func TestAdminHealthSnoozeAuthLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)

	admin := cliSeedUser(t, pool, true)
	nonAdmin := cliSeedUser(t, pool, false)
	adminUza := cliMintToken(t, pool, admin, clitoken.ScopeAdminRO)
	adminUzc := cliMintToken(t, pool, admin, clitoken.ScopeUser)

	const path = "/api/admin/health/snooze"

	// A uza_ / uzc_ CLI token is a Bearer request with no session cookie: the cookie-only
	// write group's RequireAuth rejects it with exactly 401 (the missing cookie fails before
	// CSRF or the handler), the house convention for a Bearer on a cookie-only write route.
	if rec := bearerReq(router, http.MethodPost, path, adminUza); rec.Code != http.StatusUnauthorized {
		t.Errorf("uza_ POST %s = %d, want 401 (cookie-only write group)\nbody: %s", path, rec.Code, rec.Body.String())
	}
	if rec := bearerReq(router, http.MethodPost, path, adminUzc); rec.Code != http.StatusUnauthorized {
		t.Errorf("uzc_ POST %s = %d, want 401 (cookie-only write group)\nbody: %s", path, rec.Code, rec.Body.String())
	}
	// A non-admin session cookie passes RequireAuth but RequireAdmin → 403.
	if rec := cookieReq(t, router, http.MethodPost, path, cliMintJWT(t, pool, nonAdmin), ""); rec.Code != http.StatusForbidden {
		t.Errorf("non-admin cookie POST %s = %d, want 403\nbody: %s", path, rec.Code, rec.Body.String())
	}

	// No open episode → 409 (there is nothing to snooze).
	healthClearEpisodes(t, pool)
	if rec := cookieReq(t, router, http.MethodPost, path, cliMintJWT(t, pool, admin), ""); rec.Code != http.StatusConflict {
		t.Fatalf("admin POST %s with no open episode = %d, want 409\nbody: %s", path, rec.Code, rec.Body.String())
	}

	// With an open episode: admin cookie+CSRF → 200 and a snooze row is written for that admin.
	episode := healthOpenEpisode(t, pool)
	rec := cookieReq(t, router, http.MethodPost, path, cliMintJWT(t, pool, admin), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("admin POST %s with an open episode = %d, want 200\nbody: %s", path, rec.Code, rec.Body.String())
	}
	var until time.Time
	if err := pool.QueryRow(context.Background(),
		`SELECT snoozed_until FROM health_banner_snoozes WHERE episode_id = $1 AND user_id = $2`,
		episode, admin).Scan(&until); err != nil {
		t.Fatalf("no snooze row written for (episode, admin): %v", err)
	}
	if !until.After(time.Now().Add(30 * time.Minute)) {
		t.Errorf("snoozed_until = %v, want ~1h in the future", until)
	}
}

// TestAdminHealthEpisodeAndSnoozePerCallerLiveDB proves GetAdminHealth carries the open
// episode_id and that snoozed_until is PER CALLER — it must never be served from the 5 s
// shared check cache. After one admin snoozes, that admin's document carries snoozed_until
// while a different admin's carries null, for the same episode.
func TestAdminHealthEpisodeAndSnoozePerCallerLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)

	admin1 := cliSeedUser(t, pool, true)
	admin2 := cliSeedUser(t, pool, true)
	uza1 := cliMintToken(t, pool, admin1, clitoken.ScopeAdminRO)
	uza2 := cliMintToken(t, pool, admin2, clitoken.ScopeAdminRO)

	healthClearEpisodes(t, pool)
	episode := healthOpenEpisode(t, pool)

	type healthDoc struct {
		EpisodeID    *string `json:"episode_id"`
		SnoozedUntil *string `json:"snoozed_until"`
	}
	get := func(token string) healthDoc {
		rec := bearerReq(router, http.MethodGet, "/api/admin/health", token)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/admin/health = %d, want 200\nbody: %s", rec.Code, rec.Body.String())
		}
		var d healthDoc
		if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
			t.Fatalf("decode health doc: %v (body %s)", err, rec.Body.String())
		}
		return d
	}

	// Before any snooze: both admins see the open episode id, neither is snoozed.
	if d := get(uza1); d.EpisodeID == nil || *d.EpisodeID != episode.String() {
		t.Fatalf("admin1 episode_id = %v, want %s", d.EpisodeID, episode)
	} else if d.SnoozedUntil != nil {
		t.Fatalf("admin1 snoozed_until = %v, want null before snoozing", *d.SnoozedUntil)
	}

	// admin1 snoozes through the endpoint (cookie+CSRF write).
	if rec := cookieReq(t, router, http.MethodPost, "/api/admin/health/snooze", cliMintJWT(t, pool, admin1), ""); rec.Code != http.StatusOK {
		t.Fatalf("admin1 snooze = %d, want 200\nbody: %s", rec.Code, rec.Body.String())
	}

	// Now admin1's document carries snoozed_until; admin2's (same episode) is still null.
	// This is what proves the per-caller field is NOT served from the shared check cache.
	if d := get(uza1); d.SnoozedUntil == nil {
		t.Fatalf("admin1 snoozed_until = null after snoozing, want a timestamp")
	}
	if d := get(uza2); d.EpisodeID == nil || *d.EpisodeID != episode.String() {
		t.Fatalf("admin2 episode_id = %v, want %s (same open episode)", d.EpisodeID, episode)
	} else if d.SnoozedUntil != nil {
		t.Fatalf("admin2 snoozed_until = %v, want null (admin1's snooze must not leak to admin2)", *d.SnoozedUntil)
	}
}

// TestControllerStatusAdvancesSingletonEndToEndLiveDB is the folded-in singleton test (the
// M2-A reviewers' gap). It drives the REAL ControllerStatus handler with a ZERO-worker
// report against a live DB, asserts the controller_report_status singleton advances, then
// asserts GetAdminHealth's controller.report check reads it fresh (ok — not unknown/danger).
// It pins the end-to-end path M2-A only proved at the store layer, and FAILS if the singleton
// write were gated on len(req.Workers): a zero-worker report would then leave no row, and
// controller.report would read danger (never reported, boot grace elapsed) instead of ok.
func TestControllerStatusAdvancesSingletonEndToEndLiveDB(t *testing.T) {
	h, router, pool := cliLiveDB(t)
	// Configure hosted workers so controller.report is not `na` — otherwise the check would
	// short-circuit before ever reading the singleton and the assertion would be vacuous.
	h.cfg.HostedWorkerVersion = "0.84.0"

	admin := cliSeedUser(t, pool, true)
	adminUza := cliMintToken(t, pool, admin, clitoken.ScopeAdminRO)

	// Baseline: no report yet.
	cliMustExec(t, pool, `DELETE FROM controller_report_status`)

	// Drive the real handler with a ZERO-worker report.
	body := `{"reported_at":"2026-07-26T10:00:00Z","poll_interval_seconds":10,"worker_image_tag":"0.84.0","workers":[]}`
	rec := httptest.NewRecorder()
	h.ControllerStatus(rec, httptest.NewRequest(http.MethodPost, "/api/controller/status", strings.NewReader(body)))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("ControllerStatus zero-worker report = %d, want 204\nbody: %s", rec.Code, rec.Body.String())
	}

	// The singleton advanced even though the report carried no workers.
	var observed time.Time
	if err := pool.QueryRow(context.Background(),
		`SELECT observed_at FROM controller_report_status WHERE id = 1`).Scan(&observed); err != nil {
		t.Fatalf("controller_report_status singleton did not advance on a zero-worker report: %v", err)
	}
	if time.Since(observed) > time.Minute {
		t.Fatalf("singleton observed_at = %v, want ~now (the api's receipt clock)", observed)
	}

	// GetAdminHealth's controller.report check reads the fresh singleton → ok.
	hrec := bearerReq(router, http.MethodGet, "/api/admin/health", adminUza)
	if hrec.Code != http.StatusOK {
		t.Fatalf("GET /api/admin/health = %d, want 200\nbody: %s", hrec.Code, hrec.Body.String())
	}
	var doc struct {
		Checks []struct {
			ID       string `json:"id"`
			Severity string `json:"severity"`
			Summary  string `json:"summary"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(hrec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode health doc: %v (body %s)", err, hrec.Body.String())
	}
	var found bool
	for _, c := range doc.Checks {
		if c.ID == "controller.report" {
			found = true
			if c.Severity != "ok" {
				t.Fatalf("controller.report = %q (%q), want ok after a fresh zero-worker report; "+
					"a danger/unknown here means the singleton write was gated on len(req.Workers)", c.Severity, c.Summary)
			}
		}
	}
	if !found {
		t.Fatalf("controller.report check missing from the health doc\nbody: %s", hrec.Body.String())
	}
}
