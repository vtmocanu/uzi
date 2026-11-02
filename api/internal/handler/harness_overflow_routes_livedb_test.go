package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
)

func TestHarnessOverflowRouteDifferentialAuthLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	owner, other := cliSeedUser(t, pool, false), cliSeedUser(t, pool, true)
	token := cliMintToken(t, pool, owner, clitoken.ScopeUser)
	otherToken := cliMintToken(t, pool, other, clitoken.ScopeUser)
	jwt := cliMintJWT(t, pool, owner)
	const path = "/api/me/harness-overflow"
	const enabled = `{"harness_overflow_enabled":true}`
	const disabled = `{"harness_overflow_enabled":false}`
	stored := func(user uuid.UUID) bool {
		t.Helper()
		var value bool
		if err := pool.QueryRow(context.Background(), "SELECT harness_overflow_enabled FROM users WHERE id=$1", user).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	assertSettings := func(auth string, want bool) {
		t.Helper()
		rec := bearerReq(router, http.MethodGet, "/api/me/settings", auth)
		var body struct {
			Settings struct {
				Enabled *bool `json:"harness_overflow_enabled"`
			} `json:"settings"`
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("settings=%d %s", rec.Code, rec.Body)
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Settings.Enabled == nil || *body.Settings.Enabled != want {
			t.Fatalf("settings want %v: %s (%v)", want, rec.Body, err)
		}
	}
	if stored(owner) || stored(other) {
		t.Fatal("new preference must be off")
	}
	assertSettings(token, false)
	if rec := bearerReqBody(router, http.MethodPut, path, "", enabled); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous=%d %s", rec.Code, rec.Body)
	}
	if rec := cookieNoCSRFReq(router, http.MethodPut, path, jwt, enabled); rec.Code != http.StatusForbidden {
		t.Fatalf("cookie without CSRF=%d %s", rec.Code, rec.Body)
	}
	if stored(owner) {
		t.Fatal("refused request wrote preference")
	}
	rec := bearerReqBody(router, http.MethodPut, path, token, enabled)
	if rec.Code != http.StatusOK {
		t.Fatalf("uzc bearer=%d %s", rec.Code, rec.Body)
	}
	var response struct {
		Enabled *bool `json:"harness_overflow_enabled"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil || response.Enabled == nil || !*response.Enabled {
		t.Fatalf("PUT response=%s (%v)", rec.Body, err)
	}
	if !stored(owner) || stored(other) {
		t.Fatal("bearer write not scoped to owner")
	}
	assertSettings(token, true)
	assertSettings(otherToken, false)
	for _, bad := range []string{"", `{}`, `null`, `{"harness_overflow_enabled":null}`, `{"harness_overflow_enabled":"true"}`, `{"harness_overflow_enabled":1}`, `[]`, "not json"} {
		if rec := bearerReqBody(router, http.MethodPut, path, token, bad); rec.Code != http.StatusBadRequest {
			t.Errorf("body %q=%d %s", bad, rec.Code, rec.Body)
		}
		if !stored(owner) || stored(other) {
			t.Fatal("invalid request changed preferences")
		}
	}
	if rec := cookieReq(t, router, http.MethodPut, path, jwt, disabled); rec.Code != http.StatusOK {
		t.Fatalf("cookie+CSRF=%d %s", rec.Code, rec.Body)
	}
	if stored(owner) {
		t.Fatal("explicit false not stored")
	}
	// An admin's user-scoped credential still writes only its own row.
	if rec := bearerReqBody(router, http.MethodPut, path, otherToken, enabled); rec.Code != http.StatusOK {
		t.Fatalf("other owner=%d %s", rec.Code, rec.Body)
	}
	if stored(owner) || !stored(other) {
		t.Fatal("admin write crossed owners")
	}
	assertSettings(token, false)
	assertSettings(otherToken, true)
}

func TestHarnessDecisionReadersLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	owner, admin, foreign := cliSeedUser(t, pool, false), cliSeedUser(t, pool, true), cliSeedUser(t, pool, false)
	token := cliMintToken(t, pool, owner, clitoken.ScopeUser)
	adminToken := cliMintToken(t, pool, admin, clitoken.ScopeAdminRO)
	foreignToken := cliMintToken(t, pool, foreign, clitoken.ScopeUser)
	run := seedSummaryRun(t, pool, owner, 0)
	legacy := seedSummaryRun(t, pool, owner, 1)
	cliMustExec(t, pool, `UPDATE runs SET harness='codex',harness_preferred='claude',
 harness_decision_reason='overflow',harness_preferred_score=100,harness_other_score=0,
 harness_preferred_measured=NULL,harness_other_measured=0,
 harness_preferred_held=true,harness_other_held=false WHERE id=$1`, run.id)
	assertDecision := func(raw json.RawMessage, want bool) {
		t.Helper()
		if len(raw) == 0 {
			t.Fatal("missing harness_decision key")
		}
		var d *apitypes.HarnessDecisionDTO
		if err := json.Unmarshal(raw, &d); err != nil {
			t.Fatal(err)
		}
		if !want {
			if d != nil {
				t.Fatalf("private/legacy decision leaked: %s", raw)
			}
			return
		}
		if d == nil || d.Preferred != "claude" || d.Chosen != "codex" || d.Reason != "overflow" ||
			d.PreferredScore == nil || *d.PreferredScore != 100 || d.OtherScore == nil || *d.OtherScore != 0 ||
			d.PreferredMeasured != nil || d.OtherMeasured == nil || *d.OtherMeasured != 0 || !d.PreferredHeld || d.OtherHeld {
			t.Fatalf("decision lost values/nullability: %s", raw)
		}
	}
	for _, tc := range []struct {
		name, auth string
		id         uuid.UUID
		status     int
		want       bool
	}{
		{"owner", token, run.id, http.StatusOK, true},
		{"admin", adminToken, run.id, http.StatusOK, false},
		{"foreign", foreignToken, run.id, http.StatusNotFound, false},
		{"legacy", token, legacy.id, http.StatusOK, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := bearerReq(router, http.MethodGet, "/api/runs/"+tc.id.String(), tc.auth)
			if rec.Code != tc.status {
				t.Fatalf("detail=%d %s", rec.Code, rec.Body)
			}
			if rec.Code != http.StatusOK {
				return
			}
			var body struct {
				Run map[string]json.RawMessage `json:"run"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			assertDecision(body.Run["harness_decision"], tc.want)
		})
	}
	for _, tc := range []struct {
		path, auth string
		want       bool
	}{
		{"/api/runs", token, true}, {"/api/runs?view=summary", token, true},
		{"/api/admin/runs", adminToken, false}, {"/api/admin/runs?view=summary", adminToken, false},
	} {
		body := liveList(t, router, tc.path, tc.auth)
		seen, seenLegacy := false, false
		for _, row := range body.Runs {
			var id string
			if err := json.Unmarshal(row["id"], &id); err != nil {
				t.Fatal(err)
			}
			if id == run.id.String() {
				seen = true
				assertDecision(row["harness_decision"], tc.want)
			}
			if id == legacy.id.String() {
				seenLegacy = true
				assertDecision(row["harness_decision"], false)
			}
		}
		if !seen || !seenLegacy {
			t.Fatalf("%s missed seeded decision/legacy rows", tc.path)
		}
	}
}
