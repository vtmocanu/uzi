package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/clitoken"
)

// TestUsageReserveCLIBearerLiveDB is the router-level differential-auth proof for PRD #3522:
// PUT /api/me/usage-reserve is mounted in the RequireUser group, so a uzc_ CLI Bearer reaches
// it (a cookie-only RequireAuth mount would 401 the Bearer before the handler ran, which a
// direct-handler test cannot catch), and no credential at all is refused. It also round-trips
// through GET /api/me/settings and the users table.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; ./e2e/run-store-it.sh
// provides one and sweeps this package for the LiveDB suffix.
func TestUsageReserveCLIBearerLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	owner := cliSeedUser(t, pool, false)
	uzc := cliMintToken(t, pool, owner, clitoken.ScopeUser)

	type reserve struct {
		Pct          *int `json:"weekly_reserve_pct"`
		PauseRunning bool `json:"weekly_reserve_pause_running"`
	}
	getSettings := func() reserve {
		t.Helper()
		rec := bearerReq(router, http.MethodGet, "/api/me/settings", uzc)
		if rec.Code != http.StatusOK {
			t.Fatalf("Bearer GET settings = %d\nbody: %s", rec.Code, rec.Body.String())
		}
		var out struct {
			Settings reserve `json:"settings"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode settings: %v", err)
		}
		return out.Settings
	}

	// Fresh user: reserve off, pause toggle on by default.
	if s := getSettings(); s.Pct != nil || !s.PauseRunning {
		t.Fatalf("fresh settings = %+v, want off with pause_running true", s)
	}

	rec := bearerReqBody(router, http.MethodPut, "/api/me/usage-reserve", uzc, `{"threshold_pct":70,"pause_running":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("Bearer PUT usage-reserve = %d, want 200 (RequireUser mount, NOT the 401 a cookie-only mount returns)\nbody: %s", rec.Code, rec.Body.String())
	}
	if s := getSettings(); s.Pct == nil || *s.Pct != 70 || s.PauseRunning {
		t.Fatalf("settings after PUT = %+v, want 70 / pause_running false", s)
	}

	rec = bearerReqBody(router, http.MethodPut, "/api/me/usage-reserve", uzc, `{"threshold_pct":null,"pause_running":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("Bearer PUT usage-reserve null = %d\nbody: %s", rec.Code, rec.Body.String())
	}
	if s := getSettings(); s.Pct != nil || !s.PauseRunning {
		t.Fatalf("settings after clearing = %+v, want off", s)
	}

	rec = bearerReqBody(router, http.MethodPut, "/api/me/usage-reserve", uzc, `{"threshold_pct":0,"pause_running":true}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("Bearer PUT threshold 0 = %d, want 400", rec.Code)
	}

	rec = bearerReqBody(router, http.MethodPut, "/api/me/usage-reserve", "", `{"threshold_pct":70,"pause_running":true}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no-credential PUT usage-reserve = %d, want 401", rec.Code)
	}
}

// TestUsersWeeklyReservePctCheckLiveDB: the DB CHECK is the second wall behind API
// validation. 0 and 101 are rejected, 1 and 100 and NULL accepted.
func TestUsersWeeklyReservePctCheckLiveDB(t *testing.T) {
	_, _, pool := cliLiveDB(t)
	owner := cliSeedUser(t, pool, false)
	ctx := context.Background()
	for _, bad := range []int{0, 101, -1} {
		if _, err := pool.Exec(ctx, `UPDATE users SET weekly_reserve_pct = $2 WHERE id = $1`, owner, bad); err == nil {
			t.Errorf("weekly_reserve_pct = %d accepted by the DB, want the CHECK to reject it", bad)
		}
	}
	for _, good := range []any{1, 100, nil} {
		if _, err := pool.Exec(ctx, `UPDATE users SET weekly_reserve_pct = $2 WHERE id = $1`, owner, good); err != nil {
			t.Errorf("weekly_reserve_pct = %v rejected: %v", good, err)
		}
	}
}
