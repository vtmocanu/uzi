package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/auth"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
)

func maintenanceKeys(t *testing.T, raw []byte) []string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("not a JSON object: %s", raw)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func maintenanceEvents(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT action FROM instance_intake_pause_events ORDER BY at, id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			t.Fatal(err)
		}
		out = append(out, a)
	}
	return out
}

func maintenanceAdminBody(t *testing.T, rec *httptest.ResponseRecorder) apitypes.MaintenanceAdminDTO {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("response = %d %s, want 200", rec.Code, rec.Body.String())
	}
	var dto apitypes.MaintenanceAdminDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatal(err)
	}
	return dto
}

// Mounted-route proof of the maintenance routes: auth matrix, CSRF, transitions, sanitizing
// and the exact JSON shapes the web UI consumes.
func TestMaintenanceMountedLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	reset := func() {
		cliMustExec(t, pool, `INSERT INTO instance_intake_pause (id) VALUES (true)
			ON CONFLICT (id) DO UPDATE SET active=false, since=NULL, by_user_id=NULL, by_label=NULL, reason=NULL`)
		cliMustExec(t, pool, `DELETE FROM instance_intake_pause_events`)
	}
	reset()
	t.Cleanup(reset)
	admin := cliSeedUser(t, pool, true)
	user := cliSeedUser(t, pool, false)
	adminJWT, userJWT := cliMintJWT(t, pool, admin), cliMintJWT(t, pool, user)
	adminRO := cliMintToken(t, pool, admin, clitoken.ScopeAdminRO)
	userTok := cliMintToken(t, pool, user, clitoken.ScopeUser)
	adminEmail := "cli-" + admin.String() + "@e2e"

	put := func(jwt, body string) *httptest.ResponseRecorder {
		return cookieReq(t, router, http.MethodPut, "/api/admin/maintenance", jwt, body)
	}

	// Auth matrix.
	if rec := bearerReq(router, http.MethodGet, "/api/maintenance", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET /api/maintenance = %d", rec.Code)
	}
	if rec := bearerReq(router, http.MethodGet, "/api/admin/maintenance", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated admin GET = %d", rec.Code)
	}
	if rec := bearerReqBody(router, http.MethodPut, "/api/admin/maintenance", "", `{"active":true,"reason":"x"}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated PUT = %d", rec.Code)
	}
	if rec := put(userJWT, `{"active":true,"reason":"x"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin PUT = %d %s, want 403", rec.Code, rec.Body.String())
	}
	if rec := cookieReq(t, router, http.MethodGet, "/api/admin/maintenance", userJWT, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin admin GET = %d, want 403", rec.Code)
	}
	if rec := bearerReq(router, http.MethodGet, "/api/admin/maintenance", userTok); rec.Code != http.StatusForbidden {
		t.Fatalf("user-scope token admin GET = %d, want 403", rec.Code)
	}
	// An admin read token reads, but the write group is cookie-only.
	if rec := bearerReq(router, http.MethodGet, "/api/admin/maintenance", adminRO); rec.Code != http.StatusOK {
		t.Fatalf("admin read token GET = %d %s, want 200", rec.Code, rec.Body.String())
	}
	if rec := bearerReqBody(router, http.MethodPut, "/api/admin/maintenance", adminRO, `{"active":true,"reason":"x"}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("admin read token PUT = %d %s, want 401", rec.Code, rec.Body.String())
	}
	// CSRF: a cookie session without the header is refused.
	req := httptest.NewRequest(http.MethodPut, "/api/admin/maintenance", strings.NewReader(`{"active":true,"reason":"x"}`))
	req.AddCookie(&http.Cookie{Name: auth.AuthCookieName, Value: adminJWT}) //nolint:gosec // Test client cookie.
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("PUT without CSRF = %d, want 403", rec.Code)
	}
	if ev := maintenanceEvents(t, pool); len(ev) != 0 {
		t.Fatalf("refused requests wrote events: %v", ev)
	}
	// Malformed bodies.
	for _, body := range []string{`{`, `not json`, `{"reason":"x"}`, `{"active":"yes"}`, `{"active":true,"extra":1}`} {
		if rec := put(adminJWT, body); rec.Code != http.StatusBadRequest {
			t.Fatalf("PUT %q = %d, want 400", body, rec.Code)
		}
	}

	// Off state, exact shapes.
	rec = cookieReq(t, router, http.MethodGet, "/api/admin/maintenance", adminJWT, "")
	if got := maintenanceKeys(t, rec.Body.Bytes()); !reflect.DeepEqual(got, []string{"readout", "state"}) {
		t.Fatalf("admin top-level keys = %v", got)
	}
	var top map[string]json.RawMessage
	_ = json.Unmarshal(rec.Body.Bytes(), &top)
	if got := maintenanceKeys(t, top["state"]); !reflect.DeepEqual(got, []string{"active", "by_label", "reason", "since"}) {
		t.Fatalf("admin state keys = %v", got)
	}
	if got := maintenanceKeys(t, top["readout"]); !reflect.DeepEqual(got,
		[]string{"active", "as_of", "clear", "custody", "held", "idle_bound_workers", "queued"}) {
		t.Fatalf("readout keys = %v", got)
	}
	var readout map[string]json.RawMessage
	_ = json.Unmarshal(top["readout"], &readout)
	for _, sec := range []string{"active", "held", "custody", "queued", "idle_bound_workers"} {
		if got := maintenanceKeys(t, readout[sec]); !reflect.DeepEqual(got, []string{"count", "items"}) {
			t.Fatalf("%s keys = %v", sec, got)
		}
		var s struct {
			Items json.RawMessage `json:"items"`
		}
		_ = json.Unmarshal(readout[sec], &s)
		if string(s.Items) == "null" {
			t.Fatalf("%s items must be [] not null", sec)
		}
	}
	if dto := maintenanceAdminBody(t, rec); dto.State.Active || dto.State.Since != nil || dto.State.Reason != nil || dto.State.ByLabel != nil {
		t.Fatalf("initial state: %+v", dto.State)
	}

	// Pause on.
	on := maintenanceAdminBody(t, put(adminJWT, `{"active":true,"reason":"upgrading the database"}`))
	if !on.State.Active || on.State.Since == nil || on.State.Reason == nil || *on.State.Reason != "upgrading the database" ||
		on.State.ByLabel == nil || *on.State.ByLabel != adminEmail {
		t.Fatalf("on state: %+v", on.State)
	}
	if on.Readout.AsOf == "" {
		t.Fatal("readout as_of empty")
	}
	// Idempotent PUT keeps since and writes no event.
	again := maintenanceAdminBody(t, put(adminJWT, `{"active":true,"reason":"upgrading the database"}`))
	if again.State.Since == nil || *again.State.Since != *on.State.Since {
		t.Fatalf("idempotent PUT moved since: %v vs %v", again.State.Since, on.State.Since)
	}
	if ev := maintenanceEvents(t, pool); !reflect.DeepEqual(ev, []string{"on"}) {
		t.Fatalf("events after idempotent PUT = %v", ev)
	}
	// Reason edit keeps since and attribution.
	edit := maintenanceAdminBody(t, put(adminJWT, `{"active":true,"reason":"back at noon"}`))
	if *edit.State.Since != *on.State.Since || edit.State.Reason == nil || *edit.State.Reason != "back at noon" ||
		edit.State.ByLabel == nil || *edit.State.ByLabel != adminEmail {
		t.Fatalf("reason edit: %+v", edit.State)
	}
	// ANSI and an over-long reason are sanitized, never rejected.
	long := maintenanceAdminBody(t, put(adminJWT, `{"active":true,"reason":"\u001b[31mred\u001b[0m `+strings.Repeat("é", 300)+`"}`))
	if long.State.Reason == nil || utf8.RuneCountInString(*long.State.Reason) > 280 || strings.ContainsRune(*long.State.Reason, 0x1b) ||
		!strings.HasPrefix(*long.State.Reason, "red é") {
		t.Fatalf("sanitized reason: %v", long.State.Reason)
	}

	// Public notice: exactly three keys, no attribution, for a plain user (token and session).
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"token":   bearerReq(router, http.MethodGet, "/api/maintenance", userTok),
		"session": cookieReq(t, router, http.MethodGet, "/api/maintenance", userJWT, ""),
	} {
		if rec.Code != http.StatusOK {
			t.Fatalf("%s GET /api/maintenance = %d %s", name, rec.Code, rec.Body.String())
		}
		if got := maintenanceKeys(t, rec.Body.Bytes()); !reflect.DeepEqual(got, []string{"active", "reason", "since"}) {
			t.Fatalf("%s public keys = %v", name, got)
		}
		var st apitypes.MaintenanceStateDTO
		_ = json.Unmarshal(rec.Body.Bytes(), &st)
		if !st.Active || st.Since == nil || st.Reason == nil || *st.Reason != *long.State.Reason {
			t.Fatalf("%s public state: %+v", name, st)
		}
	}

	// Off: reason ignored, since and reason cleared.
	off := maintenanceAdminBody(t, put(adminJWT, `{"active":false,"reason":"ignored"}`))
	if off.State.Active || off.State.Since != nil || off.State.Reason != nil {
		t.Fatalf("off state: %+v", off.State)
	}
	if ev := maintenanceEvents(t, pool); !reflect.DeepEqual(ev, []string{"on", "reason", "reason", "off"}) {
		t.Fatalf("events = %v", ev)
	}
	var pub map[string]any
	rec = bearerReq(router, http.MethodGet, "/api/maintenance", userTok)
	_ = json.Unmarshal(rec.Body.Bytes(), &pub)
	if pub["active"] != false || pub["since"] != nil || pub["reason"] != nil {
		t.Fatalf("public off state: %v", pub)
	}
}
