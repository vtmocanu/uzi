package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/clitoken"
)

func storedMargin(t *testing.T, pool *pgxpool.Pool, user uuid.UUID) int {
	t.Helper()
	var v int
	if err := pool.QueryRow(context.Background(), `SELECT weekly_warning_margin_pts FROM users WHERE id = $1`, user).Scan(&v); err != nil {
		t.Fatalf("read weekly_warning_margin_pts: %v", err)
	}
	return v
}

// TestWeeklyWarningMarginRouteDifferentialAuthLiveDB is the real-router proof for PRD #4123 module 6:
// PUT /api/me/weekly-warning-margin is mounted in the RequireUser group, so an anonymous request is a
// 401, a cookie write without CSRF is a 403, a uzc_ CLI Bearer works, and a cookie write with CSRF
// works. Every case reads the stored row back (a 2xx proves nothing about what was written), the
// value round-trips through GET /api/me/settings, other settings and other users are untouched, and a
// new user reads the default 15.
func TestWeeklyWarningMarginRouteDifferentialAuthLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	user := cliSeedUser(t, pool, false)
	other := cliSeedUser(t, pool, false)
	uzc := cliMintToken(t, pool, user, clitoken.ScopeUser)
	jwt := cliMintJWT(t, pool, user)
	const path = "/api/me/weekly-warning-margin"

	if m := storedMargin(t, pool, user); m != 15 {
		t.Fatalf("a new user starts at margin %d, want 15", m)
	}
	otherBefore := storedMargin(t, pool, other)

	// Another setting must survive every margin write: set a reserve through its own route first.
	if rec := bearerReqBody(router, http.MethodPut, "/api/me/usage-reserve", uzc, `{"threshold_pct":70,"pause_running":false}`); rec.Code != http.StatusOK {
		t.Fatalf("seed reserve = %d: %s", rec.Code, rec.Body)
	}

	// Anonymous: 401, nothing written.
	if rec := bearerReqBody(router, http.MethodPut, path, "", `{"margin_pts":30}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous = %d, want 401: %s", rec.Code, rec.Body)
	}
	if m := storedMargin(t, pool, user); m != 15 {
		t.Fatalf("anonymous write changed the margin to %d", m)
	}

	// Cookie without CSRF: 403, nothing written.
	if rec := cookieNoCSRFReq(router, http.MethodPut, path, jwt, `{"margin_pts":30}`); rec.Code != http.StatusForbidden {
		t.Fatalf("cookie without CSRF = %d, want 403: %s", rec.Code, rec.Body)
	}
	if m := storedMargin(t, pool, user); m != 15 {
		t.Fatalf("CSRF-less write changed the margin to %d", m)
	}

	// uzc_ Bearer: 200, the row changed, GET /api/me/settings reads it back.
	rec := bearerReqBody(router, http.MethodPut, path, uzc, `{"margin_pts":30}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("Bearer = %d, want 200 (RequireUser mount): %s", rec.Code, rec.Body)
	}
	if m := storedMargin(t, pool, user); m != 30 {
		t.Fatalf("stored margin after Bearer = %d, want 30", m)
	}
	getSettings := func() (margin int, reservePct *int) {
		t.Helper()
		rec := bearerReq(router, http.MethodGet, "/api/me/settings", uzc)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET settings = %d: %s", rec.Code, rec.Body)
		}
		var out struct {
			Settings struct {
				Margin     int  `json:"weekly_warning_margin_pts"`
				ReservePct *int `json:"weekly_reserve_pct"`
			} `json:"settings"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode settings: %v", err)
		}
		return out.Settings.Margin, out.Settings.ReservePct
	}
	if m, pct := getSettings(); m != 30 || pct == nil || *pct != 70 {
		t.Fatalf("settings after PUT: margin %d reserve %v, want 30 and 70 (other settings preserved)", m, pct)
	}

	// Cookie + CSRF: 200; 0 (off) is stored and read back.
	if rec := cookieReq(t, router, http.MethodPut, path, jwt, `{"margin_pts":0}`); rec.Code != http.StatusOK {
		t.Fatalf("cookie+CSRF = %d, want 200: %s", rec.Code, rec.Body)
	}
	if m, _ := getSettings(); m != 0 {
		t.Fatalf("settings margin after writing 0 = %d, want 0", m)
	}

	// Malformed bodies are 400 and write nothing.
	if rec := bearerReqBody(router, http.MethodPut, path, uzc, `{"margin_pts":25}`); rec.Code != http.StatusOK {
		t.Fatalf("seed 25 = %d", rec.Code)
	}
	for _, bad := range []string{`{"margin_pts":51}`, `{"margin_pts":-1}`, `{"margin_pts":1.5}`, `{"margin_pts":"9"}`, `{}`, `{"margin_pts":9,"x":1}`, `not json`} {
		if rec := bearerReqBody(router, http.MethodPut, path, uzc, bad); rec.Code != http.StatusBadRequest {
			t.Errorf("body %q = %d, want 400", bad, rec.Code)
		}
	}
	if m := storedMargin(t, pool, user); m != 25 {
		t.Fatalf("a rejected body changed the margin to %d, want 25", m)
	}
	// Identity is the session user: the other user's row never moved.
	if m := storedMargin(t, pool, other); m != otherBefore {
		t.Fatalf("another user's margin changed from %d to %d", otherBefore, m)
	}
}

// TestUsersWeeklyWarningMarginCheckLiveDB: the DB CHECK is the second wall behind API validation.
// -1 and 51 are rejected, 0 and 50 accepted, and a user row carries the default 15.
func TestUsersWeeklyWarningMarginCheckLiveDB(t *testing.T) {
	_, _, pool := cliLiveDB(t)
	owner := cliSeedUser(t, pool, false)
	ctx := context.Background()
	if m := storedMargin(t, pool, owner); m != 15 {
		t.Fatalf("default margin = %d, want 15", m)
	}
	for _, bad := range []int{-1, 51, 1000} {
		if _, err := pool.Exec(ctx, `UPDATE users SET weekly_warning_margin_pts = $2 WHERE id = $1`, owner, bad); err == nil {
			t.Errorf("weekly_warning_margin_pts = %d accepted by the DB, want the CHECK to reject it", bad)
		}
	}
	for _, good := range []int{0, 15, 50} {
		if _, err := pool.Exec(ctx, `UPDATE users SET weekly_warning_margin_pts = $2 WHERE id = $1`, owner, good); err != nil {
			t.Errorf("weekly_warning_margin_pts = %d rejected: %v", good, err)
		}
	}
}
