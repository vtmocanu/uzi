package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1910 M6 close-out: two ADR-1910 rules that had no test of their own. Skipped unless
// UZI_TEST_DATABASE_URL points at a throwaway Postgres.

// TestManualTokenWorksForAClientProductLiveDB (D2): a product that is an OAuth client still takes
// pasted uzp_ tokens, and a manual token and a connection's access token live side by side with
// neither revoking the other.
func TestManualTokenWorksForAClientProductLiveDB(t *testing.T) {
	e := tokenSetup(t)
	tok := e.connect(t)
	manual := mptMint(t, e.routes, e.jwt, e.product, "")

	e.wantWhoami(t, "the manual token of a client product", manual.Token, http.StatusOK)
	e.wantWhoami(t, "the connection's access token", tok.AccessToken, http.StatusOK)

	rec := cookieReq(t, e.routes, http.MethodDelete, mptBase+manual.ProductToken.ID, e.jwt, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("revoke the manual token = %d %q", rec.Code, rec.Body.String())
	}
	e.wantWhoami(t, "the revoked manual token", manual.Token, http.StatusUnauthorized)
	e.wantWhoami(t, "the connection's access token after the manual revoke", tok.AccessToken, http.StatusOK)
}

// TestPasswordChangeAndLogoutDoNotRevokeAConnectionLiveDB (D6): a connection outlives the user's
// session. Logout and a password change both bump users.token_version (which ends cookie sessions
// and uzc_ tokens), and neither touches a grant: the access token still answers on /api/v1 and the
// refresh token still refreshes.
func TestPasswordChangeAndLogoutDoNotRevokeAConnectionLiveDB(t *testing.T) {
	e := tokenSetup(t)
	tok := e.connect(t)
	ctx := context.Background()
	version := func() int {
		var v int
		if err := e.pool.QueryRow(ctx, `SELECT token_version FROM users WHERE id = $1`, e.user).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	stillConnected := func(step string) {
		t.Helper()
		e.wantWhoami(t, "the access token after "+step, tok.AccessToken, http.StatusOK)
		r := decodeOAuthRefresh(t, e.refresh(t, tok.RefreshToken))
		e.wantWhoami(t, "a token refreshed after "+step, r.AccessToken, http.StatusOK)
		if g := e.liveGrant(t); g.RevokedAt.Valid {
			t.Fatalf("the grant was revoked by %s", step)
		}
	}

	v0 := version()
	if rec := cookieReq(t, e.routes, http.MethodPost, "/api/auth/logout", e.jwt, ""); rec.Code != http.StatusOK {
		t.Fatalf("logout = %d %q", rec.Code, rec.Body.String())
	}
	v1 := version()
	if v1 <= v0 {
		t.Fatalf("logout did not bump token_version (%d -> %d): the test would prove nothing", v0, v1)
	}
	stillConnected("logout")

	if err := e.h.q.UpdatePassword(ctx, store.UpdatePasswordParams{ID: e.user, PasswordHash: pgtype.Text{String: "changed", Valid: true}}); err != nil {
		t.Fatalf("UpdatePassword: %v", err)
	}
	if v2 := version(); v2 <= v1 {
		t.Fatalf("a password change did not bump token_version (%d -> %d)", v1, v2)
	}
	stillConnected("a password change")
}
