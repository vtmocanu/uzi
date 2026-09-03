package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
)

// The live-DB Bearer-accept proof for PRD #1093 M2: the singleton pause resource
// (GET/PUT/DELETE /api/schedules/pause) lives under the RequireUser /schedules group so
// a uzc_ CLI Bearer reaches it (the cookie-only mis-mount trap — a RequireAuth mount
// would 401 a Bearer before the handler ever runs; a fake-client unit test cannot catch
// that). This drives the REAL h.Routes() router because the auth MOUNT is the point, and
// only the real mounted middleware chain proves which group routes_schedules.go wired the
// path into. It also exercises the full round-trip: pause, read-back, past-until 422,
// resume, read-back.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres;
// ./e2e/run-store-it.sh provides one and sweeps this package for the LiveDB suffix.
func TestSchedulePauseCLIBearerAcceptedLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	owner := cliSeedUser(t, pool, false)
	uzc := cliMintToken(t, pool, owner, clitoken.ScopeUser)

	decodePause := func(t *testing.T, body []byte) apitypes.SchedulePauseDTO {
		t.Helper()
		var dto apitypes.SchedulePauseDTO
		if err := json.Unmarshal(body, &dto); err != nil {
			t.Fatalf("decode pause DTO: %v\nbody: %s", err, body)
		}
		return dto
	}

	// (a) A Bearer GET reaches the handler (RequireUser mount) and reports not-paused for
	// a fresh user — 200, NOT the 401 a cookie-only mount would return.
	rec := bearerReq(router, http.MethodGet, "/api/schedules/pause", uzc)
	if rec.Code != http.StatusOK {
		t.Fatalf("Bearer GET pause = %d, want 200 (RequireUser mount, NOT the 401 a cookie-only mount would return)\nbody: %s", rec.Code, rec.Body.String())
	}
	if dto := decodePause(t, rec.Body.Bytes()); dto.Paused {
		t.Fatalf("fresh user GET pause: paused = true, want false")
	}

	// (b) A Bearer PUT with a future until pauses; 200 and the returned state is paused.
	future := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	rec = bearerReqBody(router, http.MethodPut, "/api/schedules/pause", uzc, fmt.Sprintf(`{"until":%q}`, future))
	if rec.Code != http.StatusOK {
		t.Fatalf("Bearer PUT pause (future) = %d, want 200\nbody: %s", rec.Code, rec.Body.String())
	}
	if dto := decodePause(t, rec.Body.Bytes()); !dto.Paused || dto.Until == nil {
		t.Fatalf("PUT pause (future): got paused=%v until=%v, want paused=true, non-nil until", dto.Paused, dto.Until)
	}

	// (c) A subsequent Bearer GET reflects the paused state.
	rec = bearerReq(router, http.MethodGet, "/api/schedules/pause", uzc)
	if rec.Code != http.StatusOK {
		t.Fatalf("Bearer GET pause after PUT = %d, want 200\nbody: %s", rec.Code, rec.Body.String())
	}
	if dto := decodePause(t, rec.Body.Bytes()); !dto.Paused {
		t.Fatalf("GET pause after PUT: paused = false, want true")
	}

	// (d) A Bearer PUT with a PAST until is a 422 (a pause already over is a client error).
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	rec = bearerReqBody(router, http.MethodPut, "/api/schedules/pause", uzc, fmt.Sprintf(`{"until":%q}`, past))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("Bearer PUT pause (past until) = %d, want 422\nbody: %s", rec.Code, rec.Body.String())
	}

	// (e) A Bearer DELETE resumes; 200 and the returned state is not paused.
	rec = bearerReq(router, http.MethodDelete, "/api/schedules/pause", uzc)
	if rec.Code != http.StatusOK {
		t.Fatalf("Bearer DELETE pause = %d, want 200\nbody: %s", rec.Code, rec.Body.String())
	}
	if dto := decodePause(t, rec.Body.Bytes()); dto.Paused {
		t.Fatalf("DELETE pause: paused = true, want false")
	}

	// (f) A subsequent Bearer GET then shows not-paused.
	rec = bearerReq(router, http.MethodGet, "/api/schedules/pause", uzc)
	if rec.Code != http.StatusOK {
		t.Fatalf("Bearer GET pause after DELETE = %d, want 200\nbody: %s", rec.Code, rec.Body.String())
	}
	if dto := decodePause(t, rec.Body.Bytes()); dto.Paused {
		t.Fatalf("GET pause after DELETE: paused = true, want false")
	}

	// (g) No credential at all → 401: the mount is Bearer-OR-cookie, not public.
	rec = bearerReq(router, http.MethodGet, "/api/schedules/pause", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no-credential GET pause = %d, want 401\nbody: %s", rec.Code, rec.Body.String())
	}
}
