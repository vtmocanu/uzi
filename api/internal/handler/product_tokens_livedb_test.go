package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/auth"
	"github.com/vtmocanu/uzi/api/internal/producttoken"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1907 M5: the user's own product tokens (/api/me/product-tokens) and the D8
// extension of Revoke all, driven through the PRODUCTION router (h.Routes via
// v1Routers), so RequireAuth's cookie+CSRF check, the route mounts and the /api/v1
// chain a minted token is then used on are all what is measured.
//
// The M5 done-criterion is TestMintUseRevokeProductTokenLiveDB (mint, 200 on whoami,
// revoke, 401) plus TestRevokeAllKillsCLIAndProductTokensLiveDB (Revoke all kills a
// live uzp_ AND a live uzc_). TestMintProductTokenCapRaceLiveDB is the D15 cap under
// concurrency: it is RED with the LockProductTokenMint call removed from
// MintProductToken.
//
// Fixtures are unique per test (fresh users and uuid-named products); the LiveDB
// packages share one database, so no assertion here is on a table-wide total.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres;
// ./e2e/run-store-it.sh provides one and sweeps this package for the LiveDB suffix.

const mptBase = "/api/me/product-tokens/"

// mptMintBody is a mint request body for productID with both scopes and the given
// expiry ("" = the server default).
func mptMintBody(productID uuid.UUID, name, expiry string) string {
	body := map[string]any{
		"product_id": productID.String(),
		"name":       name,
		"scopes":     producttoken.Scopes,
	}
	if expiry != "" {
		body["expiry"] = expiry
	}
	b, _ := json.Marshal(body)
	return string(b)
}

// mptDecodeMint strictly decodes a 201 mint response.
func mptDecodeMint(t *testing.T, rec *httptest.ResponseRecorder) apitypes.MintProductTokenResponse {
	t.Helper()
	if rec.Code != http.StatusCreated {
		t.Fatalf("mint = %d %q, want 201", rec.Code, rec.Body.String())
	}
	dec := json.NewDecoder(strings.NewReader(rec.Body.String()))
	dec.DisallowUnknownFields()
	var out apitypes.MintProductTokenResponse
	if err := dec.Decode(&out); err != nil {
		t.Fatalf("decode mint %q: %v", rec.Body.String(), err)
	}
	return out
}

// mptMint mints through POST /api/me/product-tokens/ and returns the decoded 201.
func mptMint(t *testing.T, routes http.Handler, jwt string, productID uuid.UUID, expiry string) apitypes.MintProductTokenResponse {
	t.Helper()
	return mptDecodeMint(t, cookieReq(t, routes, http.MethodPost, mptBase, jwt, mptMintBody(productID, "ci runner", expiry)))
}

// mptList returns GET /api/me/product-tokens/ (raw body plus rows by id), strictly
// decoded: a key the DTO lacks (a "token" value, a hash) fails the decode.
func mptList(t *testing.T, routes http.Handler, jwt string) (string, map[string]apitypes.ProductTokenDTO) {
	t.Helper()
	rec := cookieReq(t, routes, http.MethodGet, mptBase, jwt, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d %q, want 200", mptBase, rec.Code, rec.Body.String())
	}
	raw := rec.Body.String()
	var body struct {
		Tokens []apitypes.ProductTokenDTO `json:"tokens"`
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		t.Fatalf("decode product-token list %q: %v", raw, err)
	}
	out := make(map[string]apitypes.ProductTokenDTO, len(body.Tokens))
	for _, tk := range body.Tokens {
		out[tk.ID] = tk
	}
	return raw, out
}

// mptActiveCount reads the D15 figure straight from the table.
func mptActiveCount(t *testing.T, pool *pgxpool.Pool, userID, productID uuid.UUID) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM product_tokens
		  WHERE user_id = $1 AND product_id = $2 AND NOT revoked
		    AND (expires_at IS NULL OR expires_at > now())`, userID, productID).Scan(&n); err != nil {
		t.Fatalf("count active tokens: %v", err)
	}
	return n
}

// TestMintUseRevokeProductTokenLiveDB is the M5 done-criterion: a token minted through
// the cookie-only route is accepted by GET /api/v1/whoami, and after the user revokes it
// through DELETE /api/me/product-tokens/{id} the very next whoami is a 401. It also pins
// that the value is returned exactly once: in the mint response, never in the list.
func TestMintUseRevokeProductTokenLiveDB(t *testing.T) {
	h, pool := v1LiveDB(t)
	routes, _ := v1Routers(h)
	user := cliSeedUser(t, pool, false)
	jwt := cliMintJWT(t, pool, user)
	productID := v1SeedProduct(t, h.q, user)

	minted := mptMint(t, routes, jwt, productID, "")
	if !strings.HasPrefix(minted.Token, producttoken.Prefix) {
		t.Fatalf("minted token has no %s class prefix", producttoken.Prefix)
	}
	pt := minted.ProductToken
	if pt.ProductID != productID.String() || pt.Name != "ci runner" || pt.Revoked {
		t.Errorf("mint metadata = %+v", pt)
	}
	if !strings.HasPrefix(minted.Token, pt.TokenPrefix) || pt.TokenPrefix == minted.Token {
		t.Errorf("token_prefix %q is not a strict prefix of the minted token", pt.TokenPrefix)
	}
	if strings.Join(pt.Scopes, ",") != strings.Join(producttoken.Scopes, ",") {
		t.Errorf("scopes = %v, want %v", pt.Scopes, producttoken.Scopes)
	}
	// D10: an omitted expiry is the bounded 90-day default, set by the server.
	if pt.ExpiresAt == nil {
		t.Fatal("default mint has a NULL expires_at, want the 90d default")
	}
	if d := pt.ExpiresAt.Sub(pt.CreatedAt).Hours() / 24; d < 89.9 || d > 90.1 {
		t.Errorf("default expiry is %.2f days after creation, want 90", d)
	}

	// The list carries the row, metadata only, and never the value.
	raw, rows := mptList(t, routes, jwt)
	if _, ok := rows[pt.ID]; !ok {
		t.Fatalf("GET %s does not list the token just minted (%s)", mptBase, pt.ID)
	}
	if strings.Contains(raw, minted.Token) {
		t.Fatal("the product-token list carries the plaintext token value")
	}

	who := v1Send(routes, http.MethodGet, "/api/v1/whoami", minted.Token, nil)
	if who.status != http.StatusOK {
		t.Fatalf("whoami with the minted token = %d %q, want 200", who.status, who.body)
	}
	if got := v1DecodeWhoami(t, who.body); got.User.ID != user.String() || got.Product == nil || got.Product.ID != productID.String() {
		t.Errorf("whoami = %+v, want the minting user and the product", got)
	}

	if rec := cookieReq(t, routes, http.MethodDelete, mptBase+pt.ID, jwt, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE own token = %d %q, want 204", rec.Code, rec.Body.String())
	}
	if st := v1Send(routes, http.MethodGet, "/api/v1/whoami", minted.Token, nil).status; st != http.StatusUnauthorized {
		t.Fatalf("whoami after revoke = %d, want 401", st)
	}
	// The revoked row stays listed (the audit view), flagged revoked.
	if _, rows := mptList(t, routes, jwt); !rows[pt.ID].Revoked {
		t.Errorf("after revoke the list row = %+v, want revoked", rows[pt.ID])
	}
	// A second revoke of the same id is a 404: nothing left to revoke.
	if rec := cookieReq(t, routes, http.MethodDelete, mptBase+pt.ID, jwt, ""); rec.Code != http.StatusNotFound {
		t.Errorf("second DELETE = %d, want 404", rec.Code)
	}
}

// TestMintProductTokenExpiryChoicesLiveDB: the server turns each offered lifetime into
// expires_at, and "never" into NULL (D10).
func TestMintProductTokenExpiryChoicesLiveDB(t *testing.T) {
	h, pool := v1LiveDB(t)
	routes, _ := v1Routers(h)
	user := cliSeedUser(t, pool, false)
	jwt := cliMintJWT(t, pool, user)
	productID := v1SeedProduct(t, h.q, user)

	for expiry, days := range map[string]float64{"30d": 30, "90d": 90, "1y": 365} {
		pt := mptMint(t, routes, jwt, productID, expiry).ProductToken
		if pt.ExpiresAt == nil {
			t.Errorf("expiry %q: NULL expires_at", expiry)
			continue
		}
		if d := pt.ExpiresAt.Sub(pt.CreatedAt).Hours() / 24; d < days-0.1 || d > days+0.1 {
			t.Errorf("expiry %q: %.2f days, want %.0f", expiry, d, days)
		}
	}
	never := mptMint(t, routes, jwt, productID, "never")
	if never.ProductToken.ExpiresAt != nil {
		t.Errorf(`expiry "never": expires_at = %v, want null`, never.ProductToken.ExpiresAt)
	}
	if st := v1Send(routes, http.MethodGet, "/api/v1/whoami", never.Token, nil).status; st != http.StatusOK {
		t.Errorf("a never-expiring token on whoami = %d, want 200 (the NULL trap)", st)
	}
	// A client-set timestamp is an unknown field: 400, nothing minted.
	before := mptActiveCount(t, pool, user, productID)
	body := `{"product_id":"` + productID.String() + `","name":"n","scopes":["jobs:read"],"expires_at":"2099-01-01T00:00:00Z"}`
	if rec := cookieReq(t, routes, http.MethodPost, mptBase, jwt, body); rec.Code != http.StatusBadRequest {
		t.Errorf("mint with expires_at = %d %q, want 400", rec.Code, rec.Body.String())
	}
	if after := mptActiveCount(t, pool, user, productID); after != before {
		t.Errorf("a refused mint changed the active count %d -> %d", before, after)
	}
}

// TestRevokeAllKillsCLIAndProductTokensLiveDB: the existing panic button (POST
// /api/me/cli-tokens/revoke-all) revokes a live uzp_ AND a live uzc_ of the caller
// (D8), both refused on the next /api/v1/whoami, and leaves another user's tokens alone.
func TestRevokeAllKillsCLIAndProductTokensLiveDB(t *testing.T) {
	h, pool := v1LiveDB(t)
	routes, _ := v1Routers(h)
	user := cliSeedUser(t, pool, false)
	other := cliSeedUser(t, pool, false)
	jwt := cliMintJWT(t, pool, user)
	productID := v1SeedProduct(t, h.q, user)

	uzp := mptMint(t, routes, jwt, productID, "never").Token
	uzc := cliMintToken(t, pool, user, "user")
	otherUzp := v1MintProductToken(t, h.q, other, productID, producttoken.Scopes, nil).token
	otherUzc := cliMintToken(t, pool, other, "user")
	for name, tok := range map[string]string{"uzp_": uzp, "uzc_": uzc, "other uzp_": otherUzp, "other uzc_": otherUzc} {
		if st := v1Send(routes, http.MethodGet, "/api/v1/whoami", tok, nil).status; st != http.StatusOK {
			t.Fatalf("before revoke-all, %s on whoami = %d, want 200", name, st)
		}
	}

	if rec := cookieReq(t, routes, http.MethodPost, "/api/me/cli-tokens/revoke-all", jwt, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke-all = %d %q, want 204", rec.Code, rec.Body.String())
	}
	for name, tok := range map[string]string{"uzp_": uzp, "uzc_": uzc} {
		if st := v1Send(routes, http.MethodGet, "/api/v1/whoami", tok, nil).status; st != http.StatusUnauthorized {
			t.Errorf("after revoke-all, the caller's %s on whoami = %d, want 401", name, st)
		}
	}
	for name, tok := range map[string]string{"other uzp_": otherUzp, "other uzc_": otherUzc} {
		if st := v1Send(routes, http.MethodGet, "/api/v1/whoami", tok, nil).status; st != http.StatusOK {
			t.Errorf("after revoke-all, %s on whoami = %d, want 200 (caller-scoped)", name, st)
		}
	}
	// Idempotent.
	if rec := cookieReq(t, routes, http.MethodPost, "/api/me/cli-tokens/revoke-all", jwt, ""); rec.Code != http.StatusNoContent {
		t.Errorf("second revoke-all = %d, want 204", rec.Code)
	}
}

// TestMintProductTokenCapLiveDB: the D15 cap, sequentially. Ten active tokens for one
// product are allowed; the eleventh is a 409 and adds no row. A revoked token frees a
// slot, and the cap is per product.
func TestMintProductTokenCapLiveDB(t *testing.T) {
	h, pool := v1LiveDB(t)
	routes, _ := v1Routers(h)
	user := cliSeedUser(t, pool, false)
	jwt := cliMintJWT(t, pool, user)
	productID := v1SeedProduct(t, h.q, user)

	var first apitypes.ProductTokenDTO
	for i := range maxActiveProductTokensPerProduct {
		pt := mptMint(t, routes, jwt, productID, "").ProductToken
		if i == 0 {
			first = pt
		}
	}
	rec := cookieReq(t, routes, http.MethodPost, mptBase, jwt, mptMintBody(productID, "eleventh", ""))
	if rec.Code != http.StatusConflict {
		t.Fatalf("11th mint = %d %q, want 409", rec.Code, rec.Body.String())
	}
	if n := mptActiveCount(t, pool, user, productID); n != maxActiveProductTokensPerProduct {
		t.Fatalf("active tokens after the refused 11th = %d, want %d", n, maxActiveProductTokensPerProduct)
	}
	// The cap is per product: another product still mints.
	mptMint(t, routes, jwt, v1SeedProduct(t, h.q, user), "")
	// A revoke frees a slot.
	if rec := cookieReq(t, routes, http.MethodDelete, mptBase+first.ID, jwt, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke = %d", rec.Code)
	}
	mptMint(t, routes, jwt, productID, "")
	if n := mptActiveCount(t, pool, user, productID); n != maxActiveProductTokensPerProduct {
		t.Errorf("active tokens after revoke + mint = %d, want %d", n, maxActiveProductTokensPerProduct)
	}
}

// TestMintProductTokenCapRaceLiveDB is the D15 cap under concurrency. The user already
// holds cap-1 active tokens for a fresh product; two mints are released together by a
// barrier through the real router. Exactly one must be 201 and the other 409, leaving
// exactly cap active rows. Each iteration uses a fresh product so every one is a new
// race. Without MintProductToken's LockProductTokenMint both transactions count cap-1
// under READ COMMITTED, both insert, and the pair ends with cap+1 rows (the mutation
// this test exists to catch).
func TestMintProductTokenCapRaceLiveDB(t *testing.T) {
	h, pool := v1LiveDB(t)
	routes, _ := v1Routers(h)
	user := cliSeedUser(t, pool, false)
	jwt := cliMintJWT(t, pool, user)

	const iterations = 25
	for iter := range iterations {
		productID := v1SeedProduct(t, h.q, user)
		for range maxActiveProductTokensPerProduct - 1 {
			v1MintProductToken(t, h.q, user, productID, producttoken.Scopes, nil)
		}

		// Requests are built on the test goroutine (the CSRF helper may t.Fatal); the
		// goroutines only serve them.
		reqs := make([]*http.Request, 2)
		for i := range reqs {
			reqs[i] = mptCookieRequest(t, http.MethodPost, mptBase, jwt, mptMintBody(productID, fmt.Sprintf("race %d", i), ""))
		}
		codes := make([]int, len(reqs))
		var ready, done sync.WaitGroup
		start := make(chan struct{})
		for i, req := range reqs {
			ready.Add(1)
			done.Add(1)
			go func() {
				defer done.Done()
				rec := httptest.NewRecorder()
				ready.Done()
				<-start
				routes.ServeHTTP(rec, req)
				codes[i] = rec.Code
			}()
		}
		ready.Wait()
		close(start)
		done.Wait()

		created, conflict := 0, 0
		for _, c := range codes {
			switch c {
			case http.StatusCreated:
				created++
			case http.StatusConflict:
				conflict++
			}
		}
		active := mptActiveCount(t, pool, user, productID)
		if created != 1 || conflict != 1 || active != maxActiveProductTokensPerProduct {
			t.Fatalf("iteration %d: statuses %v (201 x%d, 409 x%d), %d active rows; want exactly one 201, one 409 and %d rows",
				iter, codes, created, conflict, active, maxActiveProductTokensPerProduct)
		}
	}
}

// mptCookieRequest is cookieReq's request half: the browser (cookie + CSRF) request for
// jwt, not yet served.
func mptCookieRequest(t *testing.T, method, path, jwt, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.AddCookie(&http.Cookie{Name: auth.AuthCookieName, Value: jwt}) //nolint:gosec // G124: test-only client cookie on an httptest request; Secure/HttpOnly/SameSite are response-side attributes irrelevant to a cookie a unit test sends.
	req.Header.Set(auth.CSRFHeaderName, cliCSRFHeader(t, jwt))
	req.Header.Set("Content-Type", "application/json")
	return req
}

// TestMintProductTokenProductStateLiveDB: an unknown product is a 404; a disabled or a
// soft-deleted product is a 409 with no row inserted; and the mint picker lists only
// enabled, live products.
func TestMintProductTokenProductStateLiveDB(t *testing.T) {
	h, pool := v1LiveDB(t)
	routes, _ := v1Routers(h)
	user := cliSeedUser(t, pool, false)
	jwt := cliMintJWT(t, pool, user)
	ctx := context.Background()

	live := v1SeedProduct(t, h.q, user)
	disabled := v1SeedProduct(t, h.q, user)
	if _, err := h.q.UpdateProduct(ctx, store.UpdateProductParams{ID: disabled, Enabled: pgtype.Bool{Bool: false, Valid: true}}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	deleted := v1SeedProduct(t, h.q, user)
	if n, err := h.q.SoftDeleteProduct(ctx, deleted); err != nil || n != 1 {
		t.Fatalf("soft delete: n=%d err=%v", n, err)
	}

	cases := []struct {
		name string
		id   uuid.UUID
		want int
	}{
		{"unknown product", uuid.New(), http.StatusNotFound},
		{"disabled product", disabled, http.StatusConflict},
		{"deleted product", deleted, http.StatusConflict},
	}
	for _, c := range cases {
		rec := cookieReq(t, routes, http.MethodPost, mptBase, jwt, mptMintBody(c.id, "x", ""))
		if rec.Code != c.want {
			t.Errorf("%s: mint = %d %q, want %d", c.name, rec.Code, rec.Body.String(), c.want)
		}
		var n int64
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM product_tokens WHERE user_id = $1 AND product_id = $2`, user, c.id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%s: a refused mint inserted %d rows", c.name, n)
		}
	}

	rec := cookieReq(t, routes, http.MethodGet, mptBase+"products", jwt, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET picker = %d %q", rec.Code, rec.Body.String())
	}
	var picker struct {
		Products []apitypes.MintableProductDTO `json:"products"`
	}
	dec := json.NewDecoder(rec.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&picker); err != nil {
		t.Fatalf("decode picker: %v", err)
	}
	seen := map[string]bool{}
	for _, p := range picker.Products {
		seen[p.ID] = true
	}
	if !seen[live.String()] {
		t.Errorf("the picker omits the enabled product %s", live)
	}
	if seen[disabled.String()] || seen[deleted.String()] {
		t.Errorf("the picker lists a disabled (%t) or deleted (%t) product", seen[disabled.String()], seen[deleted.String()])
	}
}

// TestRevokeForeignProductTokenLiveDB: DELETE /api/me/product-tokens/{id} is owner-
// scoped. Another user's token id is a 404 and that token keeps working; an unknown id is
// a 404; the list shows only the caller's own tokens.
func TestRevokeForeignProductTokenLiveDB(t *testing.T) {
	h, pool := v1LiveDB(t)
	routes, _ := v1Routers(h)
	user := cliSeedUser(t, pool, false)
	victim := cliSeedUser(t, pool, false)
	jwt := cliMintJWT(t, pool, user)
	productID := v1SeedProduct(t, h.q, victim)
	foreign := v1MintProductToken(t, h.q, victim, productID, producttoken.Scopes, nil)

	if rec := cookieReq(t, routes, http.MethodDelete, mptBase+foreign.tokenID.String(), jwt, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("DELETE a foreign token = %d %q, want 404", rec.Code, rec.Body.String())
	}
	if st := v1Send(routes, http.MethodGet, "/api/v1/whoami", foreign.token, nil).status; st != http.StatusOK {
		t.Fatalf("the foreign token after a refused revoke on whoami = %d, want 200", st)
	}
	if rec := cookieReq(t, routes, http.MethodDelete, mptBase+uuid.NewString(), jwt, ""); rec.Code != http.StatusNotFound {
		t.Errorf("DELETE an unknown id = %d, want 404", rec.Code)
	}
	if _, rows := mptList(t, routes, jwt); len(rows) != 0 {
		t.Errorf("a user with no tokens lists %d rows (another user's leaked?)", len(rows))
	}
}

// TestProductTokenRoutesRefuseBearerLiveDB: every /api/me/product-tokens route is
// cookie-only (D14). A live uzc_ (user scope), a live uzp_ and an admin's uza_ each get a
// 401 and cause nothing: the mint adds no row, the revoke leaves the token live.
func TestProductTokenRoutesRefuseBearerLiveDB(t *testing.T) {
	h, pool := v1LiveDB(t)
	routes, _ := v1Routers(h)
	user := cliSeedUser(t, pool, true)
	productID := v1SeedProduct(t, h.q, user)
	uzp := v1MintProductToken(t, h.q, user, productID, producttoken.Scopes, nil)
	bearers := map[string]string{
		"uzc_": cliMintToken(t, pool, user, "user"),
		"uza_": cliMintToken(t, pool, user, "admin_ro"),
		"uzp_": uzp.token,
	}
	routesUnderTest := []struct{ method, path, body string }{
		{http.MethodGet, mptBase, ""},
		{http.MethodGet, mptBase + "products", ""},
		{http.MethodPost, mptBase, mptMintBody(productID, "via bearer", "")},
		{http.MethodDelete, mptBase + uzp.tokenID.String(), ""},
	}
	before := mptActiveCount(t, pool, user, productID)
	for class, tok := range bearers {
		for _, rt := range routesUnderTest {
			rec := bearerReqBody(routes, rt.method, rt.path, tok, rt.body)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s %s with a %s Bearer = %d %q, want 401", rt.method, rt.path, class, rec.Code, rec.Body.String())
			}
		}
	}
	if after := mptActiveCount(t, pool, user, productID); after != before {
		t.Errorf("Bearer calls changed the active count %d -> %d", before, after)
	}
	if st := v1Send(routes, http.MethodGet, "/api/v1/whoami", uzp.token, nil).status; st != http.StatusOK {
		t.Errorf("the uzp_ token after Bearer revoke attempts on whoami = %d, want 200", st)
	}
}
