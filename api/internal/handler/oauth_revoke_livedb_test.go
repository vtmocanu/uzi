package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/oauthsrv"
	"github.com/vtmocanu/uzi/api/internal/producttoken"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1910 M3, D5/D6/D8: the revoke paths that reach a grant (the owner's and the admin's revoke of
// one grant token, Revoke all), the manual-token exclusions and GET /api/me/oauth-connections,
// through the PRODUCTION router against a real Postgres. Skipped unless UZI_TEST_DATABASE_URL
// points at a throwaway one.

// connectAs runs a real authorize + approve as the user holding jwt and exchanges the code, so the
// user ends with a live grant, an access token and a refresh token. mut can alter the authorize query.
func (e *tokenEnv) connectAs(t *testing.T, jwt string, mut func(url.Values)) apitypes.OAuthTokenResponse {
	t.Helper()
	code := e.approveAs(t, jwt, mut)
	return decodeOAuthToken(t, e.exchange(t, code))
}

// approveAs approves a fresh request as the user holding jwt and returns the unredeemed code.
func (e *tokenEnv) approveAs(t *testing.T, jwt string, mut func(url.Values)) string {
	t.Helper()
	id, binding := e.start(t, nil, mut)
	rec := e.consent(t, http.MethodPost, id, "/approve", jwt, binding)
	code := decodeRedirect(t, rec).Query().Get("code")
	if code == "" {
		t.Fatalf("approve redirect carries no code: %s", rec.Body.String())
	}
	return code
}

func (e *tokenEnv) tokenID(t *testing.T, token string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := e.pool.QueryRow(context.Background(), `SELECT id FROM product_tokens WHERE token_hash = $1`, producttoken.Hash(token)).Scan(&id); err != nil {
		t.Fatalf("token row: %v", err)
	}
	return id
}

func (e *tokenEnv) grantOf(t *testing.T, userID uuid.UUID) store.OauthGrant {
	t.Helper()
	var id uuid.UUID
	if err := e.pool.QueryRow(context.Background(),
		`SELECT id FROM oauth_grants WHERE user_id = $1 AND product_id = $2 ORDER BY created_at DESC LIMIT 1`, userID, e.product).Scan(&id); err != nil {
		t.Fatalf("grant of user: %v", err)
	}
	return e.grantByID(t, id)
}

func (e *tokenEnv) wantWhoami(t *testing.T, name, token string, want int) {
	t.Helper()
	if got := e.whoami(token); got.status != want {
		t.Fatalf("whoami with %s = %d %q, want %d", name, got.status, got.body, want)
	}
}

func (e *tokenEnv) liveGrantCount(t *testing.T, userID uuid.UUID) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM oauth_grants WHERE user_id = $1 AND revoked_at IS NULL`, userID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestRevokeMyProductTokenOnGrantTokenKillsGrantLiveDB (D6): the owner revoking ONE access token of
// a connection by id revokes the whole grant: its other tokens are 401, its refresh token is
// cleared, and an approved, unredeemed code is dead.
func TestRevokeMyProductTokenOnGrantTokenKillsGrantLiveDB(t *testing.T) {
	e := tokenSetup(t)
	tok1 := e.connectAs(t, e.jwt, nil)
	// A second connection round (re-consent with the same scopes keeps tok1 running), then a third
	// approval that stays unredeemed.
	tok2 := e.connectAs(t, e.jwt, nil)
	pendingCode := e.approveAs(t, e.jwt, nil)
	g := e.liveGrant(t)
	// Give the grant a live refresh token next to the unredeemed code (a real flow clears it on
	// every approval, so seed it directly) to pin that the revoke clears it.
	_, hash, prefix, err := oauthsrv.GenerateRefreshToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.h.q.SetOAuthGrantRefreshToken(context.Background(), store.SetOAuthGrantRefreshTokenParams{ID: g.ID, RefreshTokenHash: hash, RefreshTokenPrefix: prefix}); err != nil {
		t.Fatal(err)
	}
	e.wantWhoami(t, "tok1", tok1.AccessToken, http.StatusOK)
	e.wantWhoami(t, "tok2", tok2.AccessToken, http.StatusOK)

	rec := cookieReq(t, e.routes, http.MethodDelete, mptBase+e.tokenID(t, tok1.AccessToken).String(), e.jwt, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE a grant token = %d %q, want 204", rec.Code, rec.Body.String())
	}
	e.wantWhoami(t, "the revoked token", tok1.AccessToken, http.StatusUnauthorized)
	e.wantWhoami(t, "the grant's other token", tok2.AccessToken, http.StatusUnauthorized)
	rg := e.grantByID(t, g.ID)
	if !rg.RevokedAt.Valid || rg.RefreshTokenHash != nil || rg.RefreshTokenPrefix.Valid || rg.RefreshIssuedAt.Valid {
		t.Fatalf("grant after the revoke = %+v, want revoked with the refresh token cleared", rg)
	}
	if n := e.grantTokenCount(t, g.ID, `AND NOT revoked`); n != 0 {
		t.Fatalf("%d grant tokens still unrevoked", n)
	}
	requireOAuthError(t, e.exchange(t, pendingCode), http.StatusBadRequest, "invalid_grant")
	if n := e.grantTokenCount(t, g.ID, `AND NOT revoked`); n != 0 {
		t.Fatalf("%d grant tokens after the refused exchange", n)
	}
	// A revoked id is a 404 like any other.
	if rec := cookieReq(t, e.routes, http.MethodDelete, mptBase+e.tokenID(t, tok1.AccessToken).String(), e.jwt, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("second DELETE = %d, want 404", rec.Code)
	}
}

// A foreign id is a 404 and changes nothing; a manual token still revokes alone and leaves the
// user's connection live.
func TestRevokeMyProductTokenForeignGrantAndManualTokenLiveDB(t *testing.T) {
	e := tokenSetup(t)
	other := cliSeedUser(t, e.pool, false)
	otherJWT := cliMintJWT(t, e.pool, other)
	theirs := e.connectAs(t, otherJWT, nil)
	mine := e.connectAs(t, e.jwt, nil)

	if rec := cookieReq(t, e.routes, http.MethodDelete, mptBase+e.tokenID(t, theirs.AccessToken).String(), e.jwt, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("DELETE a foreign grant token = %d %q, want 404", rec.Code, rec.Body.String())
	}
	e.wantWhoami(t, "the foreign grant's token", theirs.AccessToken, http.StatusOK)
	if g := e.grantOf(t, other); g.RevokedAt.Valid || g.RefreshTokenHash == nil {
		t.Fatalf("a refused foreign revoke changed the grant: %+v", g)
	}
	if rec := cookieReq(t, e.routes, http.MethodDelete, mptBase+uuid.NewString(), e.jwt, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("DELETE an unknown id = %d, want 404", rec.Code)
	}

	manual := mptMint(t, e.routes, e.jwt, e.product, "never")
	e.wantWhoami(t, "the manual token", manual.Token, http.StatusOK)
	if rec := cookieReq(t, e.routes, http.MethodDelete, mptBase+manual.ProductToken.ID, e.jwt, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE a manual token = %d, want 204", rec.Code)
	}
	e.wantWhoami(t, "the revoked manual token", manual.Token, http.StatusUnauthorized)
	e.wantWhoami(t, "the connection's token after a manual revoke", mine.AccessToken, http.StatusOK)
	if g := e.grantOf(t, e.user); g.RevokedAt.Valid {
		t.Fatal("revoking a manual token must not revoke the user's connection")
	}
}

// TestGrantTokensAreNotManualTokensLiveDB (D5): ten live grant tokens neither block a manual mint
// (the D15 cap), nor appear in the user's or the admin's token list, nor in the active-token counts.
func TestGrantTokensAreNotManualTokensLiveDB(t *testing.T) {
	e := tokenSetup(t)
	tok := e.connectAs(t, e.jwt, nil)
	g := e.liveGrant(t)
	for i := 0; i < 9; i++ {
		e.seedGrantToken(t, g.ID, []string{"jobs:read"})
	}
	if n := e.grantTokenCount(t, g.ID, `AND NOT revoked AND expires_at > now()`); n != 10 {
		t.Fatalf("live grant tokens = %d, want 10", n)
	}
	n, err := e.h.q.CountActiveProductTokensForUserProduct(context.Background(),
		store.CountActiveProductTokensForUserProductParams{UserID: e.user, ProductID: e.product})
	if err != nil || n != 0 {
		t.Fatalf("manual active count with ten grant tokens = %d, %v; want 0", n, err)
	}

	manual := mptMint(t, e.routes, e.jwt, e.product, "")
	_, rows, _ := mptListOrdered(t, e.routes, e.jwt)
	if len(rows) != 1 || rows[0].ID != manual.ProductToken.ID {
		t.Fatalf("the user's list = %+v, want only the manual token", rows)
	}
	if n, err := e.h.q.CountActiveProductTokensForProduct(context.Background(), e.product); err != nil || n != 1 {
		t.Fatalf("CountActiveProductTokensForProduct = %d, %v; want 1 (the manual token)", n, err)
	}
	products, err := e.h.q.ListProducts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range products {
		if p.ID == e.product {
			found = true
			if p.ActiveTokenCount != 1 {
				t.Fatalf("ListProducts active_token_count = %d, want 1 (manual tokens only)", p.ActiveTokenCount)
			}
		}
	}
	if !found {
		t.Fatal("product missing from ListProducts")
	}
	admin := cliSeedUser(t, e.pool, true)
	rec := cookieReq(t, e.routes, http.MethodGet, "/api/admin/product-tokens", cliMintJWT(t, e.pool, admin), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("admin list = %d %q", rec.Code, rec.Body.String())
	}
	var body struct {
		Tokens []apitypes.AdminProductTokenDTO `json:"tokens"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, tk := range body.Tokens {
		if tk.ProductID == e.product.String() && tk.ID != manual.ProductToken.ID {
			t.Fatalf("the admin list carries a non-manual token %s of the product", tk.ID)
		}
	}
	e.wantWhoami(t, "the grant token", tok.AccessToken, http.StatusOK)
}

// TestProductConnectionCountsLiveDB (D5): a product's live OAuth connections are counted apart
// from its manual tokens. The registry list and the PATCH response carry live_connection_count
// (revoked grants excluded, active_token_count untouched), and the delete response carries
// stopped_connection_count: the live connections for a product that was enabled at deletion, 0
// for one that was already disabled (its product still reports the live ones).
func TestProductConnectionCountsLiveDB(t *testing.T) {
	setup := func(t *testing.T) (e *tokenEnv, adminJWT string) {
		e = tokenSetup(t)
		e.connectAs(t, e.jwt, nil)
		// A second user's connection that was revoked does not count.
		other := cliSeedUser(t, e.pool, false)
		otherJWT := cliMintJWT(t, e.pool, other)
		e.connectAs(t, otherJWT, nil)
		if rec := cookieReq(t, e.routes, http.MethodPost, "/api/me/cli-tokens/revoke-all", otherJWT, ""); rec.Code != http.StatusNoContent {
			t.Fatalf("revoke-all = %d", rec.Code)
		}
		return e, cliMintJWT(t, e.pool, cliSeedUser(t, e.pool, true))
	}
	listed := func(t *testing.T, e *tokenEnv, jwt string) apitypes.ProductDTO {
		t.Helper()
		got, ok := apListProducts(t, e.routes, jwt)[e.product.String()]
		if !ok {
			t.Fatal("product missing from the admin list")
		}
		return got
	}

	t.Run("an enabled product", func(t *testing.T) {
		e, jwt := setup(t)
		if got := listed(t, e, jwt); got.LiveConnectionCount != 1 || got.ActiveTokenCount != 0 {
			t.Fatalf("list: live_connection_count = %d, active_token_count = %d; want 1 and 0", got.LiveConnectionCount, got.ActiveTokenCount)
		}
		rec := cookieReq(t, e.routes, http.MethodPatch, "/api/admin/products/"+e.product.String(), jwt, `{"description":"edited"}`)
		if got := apDecodeProduct(t, rec.Code, rec.Body.String()).Product; rec.Code != http.StatusOK || got.LiveConnectionCount != 1 {
			t.Fatalf("PATCH = %d, live_connection_count = %d; want 200 and 1", rec.Code, got.LiveConnectionCount)
		}
		rec = cookieReq(t, e.routes, http.MethodDelete, "/api/admin/products/"+e.product.String(), jwt, "")
		resp := apDecodeProduct(t, rec.Code, rec.Body.String())
		if rec.Code != http.StatusOK || resp.StoppedConnectionCount == nil || *resp.StoppedConnectionCount != 1 ||
			resp.StoppedTokenCount == nil || *resp.StoppedTokenCount != 0 || resp.Product.LiveConnectionCount != 1 {
			t.Fatalf("DELETE = %d %q, want stopped_connection_count 1, stopped_token_count 0, product.live_connection_count 1", rec.Code, rec.Body.String())
		}
	})

	t.Run("an already disabled product", func(t *testing.T) {
		e, jwt := setup(t)
		if rec := cookieReq(t, e.routes, http.MethodPatch, "/api/admin/products/"+e.product.String(), jwt, `{"enabled":false}`); rec.Code != http.StatusOK {
			t.Fatalf("PATCH enabled=false = %d %q", rec.Code, rec.Body.String())
		}
		rec := cookieReq(t, e.routes, http.MethodDelete, "/api/admin/products/"+e.product.String(), jwt, "")
		resp := apDecodeProduct(t, rec.Code, rec.Body.String())
		if rec.Code != http.StatusOK || resp.StoppedConnectionCount == nil || *resp.StoppedConnectionCount != 0 || resp.Product.LiveConnectionCount != 1 {
			t.Fatalf("DELETE = %d %q, want stopped_connection_count 0 and product.live_connection_count 1", rec.Code, rec.Body.String())
		}
	})
}

// TestRevokeAllProductTokensSkipsGrantTokensLiveDB (D8): the plain sweep revokes manual tokens only.
// A grant's access token is revoked together with its grant under the grant lock; revoking it here
// alone would leave a grant created concurrently with its access token dead and its grant and
// refresh token live.
func TestRevokeAllProductTokensSkipsGrantTokensLiveDB(t *testing.T) {
	e := tokenSetup(t)
	tok := e.connectAs(t, e.jwt, nil)
	manual := mptMint(t, e.routes, e.jwt, e.product, "")
	if err := e.h.q.RevokeAllProductTokens(context.Background(), e.user); err != nil {
		t.Fatal(err)
	}
	e.wantWhoami(t, "the manual token", manual.Token, http.StatusUnauthorized)
	e.wantWhoami(t, "the grant token the plain sweep must not touch", tok.AccessToken, http.StatusOK)
	if n := e.liveGrantCount(t, e.user); n != 1 {
		t.Fatalf("%d live grants, want 1", n)
	}
}

// TestRevokeAllRevokesGrantsLiveDB (D6): Revoke all leaves no live grant for the caller, an approved
// but unredeemed code can no longer be redeemed, and another user's connection is untouched.
func TestRevokeAllRevokesGrantsLiveDB(t *testing.T) {
	e := tokenSetup(t)
	other := cliSeedUser(t, e.pool, false)
	theirs := e.connectAs(t, cliMintJWT(t, e.pool, other), nil)
	mine := e.connectAs(t, e.jwt, nil)
	pending := e.approveAs(t, e.jwt, nil)

	if rec := cookieReq(t, e.routes, http.MethodPost, "/api/me/cli-tokens/revoke-all", e.jwt, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke-all = %d %q, want 204", rec.Code, rec.Body.String())
	}
	if n := e.liveGrantCount(t, e.user); n != 0 {
		t.Fatalf("%d live grants after revoke-all, want 0", n)
	}
	g := e.grantOf(t, e.user)
	if !g.RevokedAt.Valid || g.RefreshTokenHash != nil {
		t.Fatalf("grant after revoke-all = %+v", g)
	}
	e.wantWhoami(t, "the caller's access token", mine.AccessToken, http.StatusUnauthorized)
	requireOAuthError(t, e.exchange(t, pending), http.StatusBadRequest, "invalid_grant")
	if n := e.grantTokenCount(t, g.ID, `AND NOT revoked`); n != 0 {
		t.Fatalf("%d unrevoked grant tokens after revoke-all and a refused exchange", n)
	}
	e.wantWhoami(t, "another user's access token", theirs.AccessToken, http.StatusOK)
	if n := e.liveGrantCount(t, other); n != 1 {
		t.Fatalf("another user's live grants = %d, want 1", n)
	}
	// Idempotent.
	if rec := cookieReq(t, e.routes, http.MethodPost, "/api/me/cli-tokens/revoke-all", e.jwt, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("second revoke-all = %d, want 204", rec.Code)
	}
}

// TestRevokeAllRacingCodeExchangeLiveDB (D8): a code exchange racing Revoke all leaves no live
// token. A third transaction holds the grant lock so both requests are genuinely blocked on it
// before they run; whichever wins, the exchange either completed first and is then revoked, or
// fails with invalid_grant, and the grant ends revoked either way.
func TestRevokeAllRacingCodeExchangeLiveDB(t *testing.T) {
	e := tokenSetup(t)
	ctx := context.Background()
	var exchangedFirst, refusedAfter int
	for i := 0; i < 8; i++ {
		code := e.approveAs(t, e.jwt, nil)
		g := e.liveGrant(t)
		tx, err := e.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `SELECT 1 FROM oauth_grants WHERE id = $1 FOR UPDATE`, g.ID); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var exch, rev *httptest.ResponseRecorder
		wg.Add(2)
		go func() { defer wg.Done(); exch = e.exchange(t, code) }()
		go func() {
			defer wg.Done()
			rev = cookieReq(t, e.routes, http.MethodPost, "/api/me/cli-tokens/revoke-all", e.jwt, "")
		}()
		deadline := time.Now().Add(10 * time.Second)
		for !pgActivityWaiting(t, e, "FROM oauth_grants WHERE id = $1 FOR UPDATE") || !pgActivityWaiting(t, e, "ORDER BY id") {
			if time.Now().After(deadline) {
				t.Fatal("the requests never blocked on the grant lock")
			}
			time.Sleep(10 * time.Millisecond)
		}
		time.Sleep(50 * time.Millisecond)
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		wg.Wait()
		if rev.Code != http.StatusNoContent {
			t.Fatalf("revoke-all = %d %q", rev.Code, rev.Body.String())
		}
		switch exch.Code {
		case http.StatusOK:
			exchangedFirst++
			e.wantWhoami(t, "a token minted before revoke-all", decodeOAuthToken(t, exch).AccessToken, http.StatusUnauthorized)
		case http.StatusBadRequest:
			refusedAfter++
			requireOAuthError(t, exch, http.StatusBadRequest, "invalid_grant")
		default:
			t.Fatalf("exchange = %d %q, want 200 (then revoked) or invalid_grant", exch.Code, exch.Body.String())
		}
		if n := e.grantTokenCount(t, g.ID, `AND NOT revoked`); n != 0 {
			t.Fatalf("iteration %d: %d unrevoked grant tokens survive revoke-all", i, n)
		}
		if n := e.liveGrantCount(t, e.user); n != 0 {
			t.Fatalf("iteration %d: %d live grants survive revoke-all", i, n)
		}
	}
	t.Logf("exchange won %d times, revoke-all won %d times", exchangedFirst, refusedAfter)
}

// pgWaitingCount counts the backends waiting on a lock whose current statement contains fragment.
func pgWaitingCount(t *testing.T, e *tokenEnv, fragment string) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND query LIKE '%' || $1 || '%'`, fragment).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// waitForPgWaiting blocks until want backends wait on a lock inside a statement containing fragment.
func waitForPgWaiting(t *testing.T, e *tokenEnv, fragment string, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for pgWaitingCount(t, e, fragment) < want {
		if time.Now().After(deadline) {
			t.Fatalf("%d backends never waited on a lock in %q", want, fragment)
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
}

// TestRevokeAllVersusFirstConsentApproveLiveDB (D8): a first-consent approve that has inserted its
// grant but not committed is invisible to every row lock, so Revoke all serializes with it on the
// per-user lock (store.OAuthUserLockClass). Both orderings are pinned:
//
//   - the approve took the lock first: Revoke all waits for its commit and then revokes the new
//     grant, so afterwards there is no live grant and its code cannot be redeemed;
//   - Revoke all took the lock first: the approve runs after it and creates a live grant of its
//     own, a consent given after the button was pressed, which works normally.
func TestRevokeAllVersusFirstConsentApproveLiveDB(t *testing.T) {
	ctx := context.Background()
	revokeAll := func(e *tokenEnv, out **httptest.ResponseRecorder, wg *sync.WaitGroup) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			*out = cookieReq(t, e.routes, http.MethodPost, "/api/me/cli-tokens/revoke-all", e.jwt, "")
		}()
	}

	t.Run("the approve holds the lock first", func(t *testing.T) {
		e := tokenSetup(t)
		id, binding := e.start(t, nil, nil)
		// Hold the request row, so the approve blocks on its claim AFTER inserting the grant and
		// while holding the per-user lock, with the grant uncommitted.
		tx, err := e.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx) //nolint:errcheck // no-op after the commit below
		if _, err := tx.Exec(ctx, `SELECT 1 FROM oauth_authorize_requests WHERE id = $1 FOR UPDATE`, id); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var appr, rev *httptest.ResponseRecorder
		wg.Add(1)
		go func() { defer wg.Done(); appr = e.consent(t, http.MethodPost, id, "/approve", e.jwt, binding) }()
		waitForPgWaiting(t, e, "name: ClaimOAuthAuthorizeRequest", 1)
		revokeAll(e, &rev, &wg)
		waitForPgWaiting(t, e, "name: LockOAuthUserGrants", 1)
		if n := e.liveGrantCount(t, e.user); n != 0 {
			t.Fatalf("the uncommitted grant is visible (%d live grants): the test no longer exercises the window", n)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		wg.Wait()
		code := decodeRedirect(t, appr).Query().Get("code")
		if code == "" || rev.Code != http.StatusNoContent {
			t.Fatalf("approve = %d, revoke-all = %d %q", appr.Code, rev.Code, rev.Body.String())
		}
		if n := e.liveGrantCount(t, e.user); n != 0 {
			t.Fatalf("%d live grants after revoke-all raced a first consent, want 0", n)
		}
		requireOAuthError(t, e.exchange(t, code), http.StatusBadRequest, "invalid_grant")
		if n := e.grantTokenCount(t, e.grantOf(t, e.user).ID, `AND NOT revoked`); n != 0 {
			t.Fatalf("%d unrevoked grant tokens", n)
		}
	})

	t.Run("revoke-all holds the lock first", func(t *testing.T) {
		e := tokenSetup(t)
		before := e.connectAs(t, e.jwt, nil)
		oldGrant := e.grantOf(t, e.user)
		id, binding := e.start(t, nil, nil)
		// Hold the per-user lock so both requests queue on it, revoke-all first.
		tx, err := e.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx) //nolint:errcheck // no-op after the commit below
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1, hashtext($2::uuid::text))`, store.OAuthUserLockClass, e.user); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var appr, rev *httptest.ResponseRecorder
		revokeAll(e, &rev, &wg)
		waitForPgWaiting(t, e, "name: LockOAuthUserGrants", 1)
		wg.Add(1)
		go func() { defer wg.Done(); appr = e.consent(t, http.MethodPost, id, "/approve", e.jwt, binding) }()
		waitForPgWaiting(t, e, "name: LockOAuthUserGrants", 2)
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		wg.Wait()
		code := decodeRedirect(t, appr).Query().Get("code")
		if code == "" || rev.Code != http.StatusNoContent {
			t.Fatalf("approve = %d, revoke-all = %d %q", appr.Code, rev.Code, rev.Body.String())
		}
		e.wantWhoami(t, "the connection revoke-all revoked", before.AccessToken, http.StatusUnauthorized)
		if n := e.liveGrantCount(t, e.user); n != 1 {
			t.Fatalf("%d live grants, want exactly the one the later approve created", n)
		}
		if g := e.grantOf(t, e.user); g.ID == oldGrant.ID {
			t.Fatal("the live grant is the one revoke-all revoked")
		}
		after := decodeOAuthToken(t, e.exchange(t, code))
		e.wantWhoami(t, "the token of the later consent", after.AccessToken, http.StatusOK)
	})
}

// TestAdminRevokeGrantTokenKillsGrantLiveDB (D6): an admin revoking one grant token by id kills the
// grant; a manual token still revokes alone; an unknown id is a 404.
func TestAdminRevokeGrantTokenKillsGrantLiveDB(t *testing.T) {
	e := tokenSetup(t)
	adminJWT := cliMintJWT(t, e.pool, cliSeedUser(t, e.pool, true))
	tok1 := e.connectAs(t, e.jwt, nil)
	tok2 := e.connectAs(t, e.jwt, nil)
	pending := e.approveAs(t, e.jwt, nil)
	g := e.liveGrant(t)

	path := func(id uuid.UUID) string { return "/api/admin/product-tokens/" + id.String() + "/revoke" }
	if rec := cookieReq(t, e.routes, http.MethodPost, path(e.tokenID(t, tok1.AccessToken)), adminJWT, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("admin revoke of a grant token = %d %q, want 204", rec.Code, rec.Body.String())
	}
	e.wantWhoami(t, "the revoked token", tok1.AccessToken, http.StatusUnauthorized)
	e.wantWhoami(t, "the grant's other token", tok2.AccessToken, http.StatusUnauthorized)
	if rg := e.grantByID(t, g.ID); !rg.RevokedAt.Valid || rg.RefreshTokenHash != nil {
		t.Fatalf("grant after the admin revoke = %+v", rg)
	}
	requireOAuthError(t, e.exchange(t, pending), http.StatusBadRequest, "invalid_grant")
	if rec := cookieReq(t, e.routes, http.MethodPost, path(e.tokenID(t, tok1.AccessToken)), adminJWT, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("admin revoke of a revoked token = %d, want 404", rec.Code)
	}
	if rec := cookieReq(t, e.routes, http.MethodPost, path(uuid.New()), adminJWT, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("admin revoke of an unknown id = %d, want 404", rec.Code)
	}

	// A manual token revokes alone and leaves a live connection live.
	tok3 := e.connectAs(t, e.jwt, nil)
	manual := mptMint(t, e.routes, e.jwt, e.product, "never")
	manualID, _ := uuid.Parse(manual.ProductToken.ID)
	if rec := cookieReq(t, e.routes, http.MethodPost, path(manualID), adminJWT, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("admin revoke of a manual token = %d, want 204", rec.Code)
	}
	e.wantWhoami(t, "the revoked manual token", manual.Token, http.StatusUnauthorized)
	e.wantWhoami(t, "the new connection's token", tok3.AccessToken, http.StatusOK)
}

// TestMyOAuthConnectionsLiveDB: the list shows the caller's live grants whatever the state of their
// access tokens, with the right timestamps, and nothing of anyone else's or of a revoked grant.
func TestMyOAuthConnectionsLiveDB(t *testing.T) {
	e := tokenSetup(t)
	path := "/api/me/oauth-connections/"
	list := func(jwt string) (string, []apitypes.OAuthConnectionDTO) {
		t.Helper()
		rec := cookieReq(t, e.routes, http.MethodGet, path, jwt, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d %q", path, rec.Code, rec.Body.String())
		}
		var body struct {
			Connections []apitypes.OAuthConnectionDTO `json:"connections"`
		}
		dec := json.NewDecoder(strings.NewReader(rec.Body.String()))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil {
			t.Fatalf("decode %q: %v", rec.Body.String(), err)
		}
		if body.Connections == nil {
			t.Fatalf("connections is null in %q, want []", rec.Body.String())
		}
		return rec.Body.String(), body.Connections
	}
	if _, rows := list(e.jwt); len(rows) != 0 {
		t.Fatalf("a user with no connection lists %d", len(rows))
	}

	other := cliSeedUser(t, e.pool, false)
	e.connectAs(t, cliMintJWT(t, e.pool, other), nil)
	tok := e.connectAs(t, e.jwt, func(v url.Values) { v.Set("scope", "jobs:read") })
	g := e.liveGrant(t)
	// Every access token of the grant expired: the connection is still live and listed.
	cliMustExec(t, e.pool, `UPDATE product_tokens SET expires_at = now() - interval '1 minute' WHERE grant_id = $1`, g.ID)
	// Last use: a token used 3 minutes ago and a refresh used 1 minute ago (the later one wins).
	cliMustExec(t, e.pool, `UPDATE product_tokens SET last_used_at = now() - interval '3 minutes' WHERE grant_id = $1`, g.ID)
	cliMustExec(t, e.pool, `UPDATE oauth_grants SET refresh_last_used_at = now() - interval '1 minute' WHERE id = $1`, g.ID)

	raw, rows := list(e.jwt)
	if len(rows) != 1 {
		t.Fatalf("connections = %+v, want exactly the caller's live grant", rows)
	}
	r := rows[0]
	if r.ID != g.ID.String() || r.ProductID != e.product.String() || r.ProductName == "" || !slices.Equal(r.Scopes, []string{"jobs:read"}) {
		t.Fatalf("connection = %+v", r)
	}
	if !r.ConnectedAt.Equal(g.ConsentedAt.Time) || !r.CreatedAt.Equal(g.CreatedAt.Time) || r.RefreshIssuedAt == nil {
		t.Fatalf("timestamps = %+v vs grant %+v", r, g)
	}
	var wantUsed time.Time
	if err := e.pool.QueryRow(context.Background(), `SELECT refresh_last_used_at FROM oauth_grants WHERE id = $1`, g.ID).Scan(&wantUsed); err != nil {
		t.Fatal(err)
	}
	if r.LastUsedAt == nil || !r.LastUsedAt.Equal(wantUsed) {
		t.Fatalf("last_used_at = %v, want the later refresh use %v", r.LastUsedAt, wantUsed)
	}
	for _, secret := range []string{tok.AccessToken, tok.RefreshToken, "token_hash", "refresh_token"} {
		if strings.Contains(raw, secret) {
			t.Fatalf("the list leaks %q: %s", secret[:min(len(secret), 8)], raw)
		}
	}
	// With no refresh use the latest access-token use is reported.
	cliMustExec(t, e.pool, `UPDATE oauth_grants SET refresh_last_used_at = NULL WHERE id = $1`, g.ID)
	if _, rows := list(e.jwt); rows[0].LastUsedAt == nil {
		t.Fatal("last_used_at is null though an access token was used")
	}
	// A revoked grant is gone from the list.
	if rec := cookieReq(t, e.routes, http.MethodPost, "/api/me/cli-tokens/revoke-all", e.jwt, ""); rec.Code != http.StatusNoContent {
		t.Fatal("revoke-all failed")
	}
	if _, rows := list(e.jwt); len(rows) != 0 {
		t.Fatalf("a revoked connection is still listed: %+v", rows)
	}
}

// TestOAuthConnectionsRouteRefusesBearerLiveDB (D14 pattern): the route is cookie-only; a uzc_, a
// uza_ and a uzp_ Bearer, an OAuth access token and a refresh token all get 401 and no data.
func TestOAuthConnectionsRouteRefusesBearerLiveDB(t *testing.T) {
	e := tokenSetup(t)
	tok := e.connectAs(t, e.jwt, nil)
	bearers := map[string]string{
		"uzc_":          cliMintToken(t, e.pool, e.user, "user"),
		"uza_":          cliMintToken(t, e.pool, e.user, "admin_ro"),
		"uzp_ (manual)": v1MintProductToken(t, e.h.q, e.user, e.product, producttoken.Scopes, nil).token,
		"uzp_ (oauth)":  tok.AccessToken,
		"uzr_":          tok.RefreshToken,
	}
	for class, bearer := range bearers {
		rec := bearerReqBody(e.routes, http.MethodGet, "/api/me/oauth-connections/", bearer, "")
		if rec.Code != http.StatusUnauthorized || strings.Contains(rec.Body.String(), e.product.String()) {
			t.Errorf("GET /api/me/oauth-connections/ with a %s Bearer = %d %q, want 401", class, rec.Code, rec.Body.String())
		}
	}
	if rec := bearerReqBody(e.routes, http.MethodGet, "/api/me/oauth-connections/", "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("no credential = %d, want 401", rec.Code)
	}
}
