package handler

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/producttoken"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1907 M4: the admin product registry and product-credential inventory, driven
// through the PRODUCTION router (h.Routes via v1Routers), so the read/write group
// split, RequireAuth's CSRF check and the /api/v1 mount are all what is measured.
//
// The M4 done-criterion is TestAdminProductActionsKillTokenOnNextWhoamiLiveDB: each of
// disable, soft delete and admin revoke turns a real minted uzp_ token from 200 to 401
// on the very next GET /api/v1/whoami, and a soft-deleted product's token rows are
// still listed.
//
// Fixtures are unique per test (uuid-derived names and users); the LiveDB packages
// share one database, so no assertion here is on a table-wide total.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres;
// ./e2e/run-store-it.sh provides one and sweeps this package for the LiveDB suffix.

type apProductResp struct {
	Product           apitypes.ProductDTO `json:"product"`
	StoppedTokenCount *int64              `json:"stopped_token_count"`
}

func apDecodeProduct(t *testing.T, code int, body string) apProductResp {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(body))
	dec.DisallowUnknownFields()
	var out apProductResp
	if err := dec.Decode(&out); err != nil {
		t.Fatalf("decode product response (status %d) %q: %v", code, body, err)
	}
	return out
}

func apCreate(t *testing.T, routes http.Handler, jwt, name, desc string) apitypes.ProductDTO {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"name": name, "description": desc})
	rec := cookieReq(t, routes, http.MethodPost, "/api/admin/products", jwt, string(body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /api/admin/products = %d %q, want 201", rec.Code, rec.Body.String())
	}
	return apDecodeProduct(t, rec.Code, rec.Body.String()).Product
}

// apListProducts returns GET /api/admin/products indexed by id.
func apListProducts(t *testing.T, routes http.Handler, jwt string) map[string]apitypes.ProductDTO {
	t.Helper()
	rec := cookieReq(t, routes, http.MethodGet, "/api/admin/products", jwt, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/admin/products = %d %q, want 200", rec.Code, rec.Body.String())
	}
	var body struct {
		Products []apitypes.ProductDTO `json:"products"`
	}
	dec := json.NewDecoder(rec.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		t.Fatalf("decode products: %v", err)
	}
	out := make(map[string]apitypes.ProductDTO, len(body.Products))
	for _, p := range body.Products {
		out[p.ID] = p
	}
	return out
}

// apListProductTokens returns GET /api/admin/product-tokens (raw body, rows by id, and
// the response's truncated flag).
func apListProductTokens(t *testing.T, routes http.Handler, jwt string) (string, map[string]apitypes.AdminProductTokenDTO, bool) {
	t.Helper()
	rec := cookieReq(t, routes, http.MethodGet, "/api/admin/product-tokens", jwt, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/admin/product-tokens = %d %q, want 200", rec.Code, rec.Body.String())
	}
	raw := rec.Body.String()
	var body struct {
		Tokens    []apitypes.AdminProductTokenDTO `json:"tokens"`
		Truncated *bool                           `json:"truncated"`
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		t.Fatalf("decode product tokens: %v", err)
	}
	if body.Truncated == nil {
		t.Fatalf("GET /api/admin/product-tokens has no truncated key: %q", raw)
	}
	out := make(map[string]apitypes.AdminProductTokenDTO, len(body.Tokens))
	for _, tk := range body.Tokens {
		out[tk.ID] = tk
	}
	return raw, out, *body.Truncated
}

// apInventoryRows returns the admin inventory rows by id for assertions that a specific
// row is listed. The route's response is used when it is complete; when it says
// truncated (the shared database holds more than maxAdminProductTokenRows tokens, and
// active-first ordering may push this test's revoked/expired rows past the cut) the
// SAME store query the handler runs is read with a bound no test database reaches, so
// the D9 claim (a deleted product's rows, revoked and expired ones included, are
// still in the inventory) stays pinned without depending on the table's global size.
func apInventoryRows(t *testing.T, q *store.Queries, httpRows map[string]apitypes.AdminProductTokenDTO, truncated bool) map[string]apitypes.AdminProductTokenDTO {
	t.Helper()
	if !truncated {
		return httpRows
	}
	t.Logf("GET /api/admin/product-tokens is truncated on this shared database; asserting the rows via ListAllProductTokensForAdmin")
	rows, err := q.ListAllProductTokensForAdmin(context.Background(), 1_000_000_000)
	if err != nil {
		t.Fatalf("ListAllProductTokensForAdmin: %v", err)
	}
	out := make(map[string]apitypes.AdminProductTokenDTO, len(rows))
	for _, r := range rows {
		out[r.ID.String()] = apitypes.AdminProductTokenDTO{
			ProductTokenDTO: apitypes.ProductTokenDTO{
				ID:          r.ID.String(),
				ProductID:   r.ProductID.String(),
				ProductName: r.ProductName,
				Name:        r.Name,
				TokenPrefix: r.TokenPrefix,
				Scopes:      r.Scopes,
				Revoked:     r.Revoked,
				CreatedAt:   r.CreatedAt.Time,
				ExpiresAt:   timePtr(r.ExpiresAt.Valid, r.ExpiresAt.Time),
			},
			UserID:     r.UserID.String(),
			OwnerEmail: r.OwnerEmail,
		}
	}
	return out
}

func apWhoami(t *testing.T, routes http.Handler, token string) int {
	t.Helper()
	return v1Send(routes, http.MethodGet, "/api/v1/whoami", token, nil).status
}

// TestAdminProductRegistryLiveDB: create \u2192 list, the 409 on a duplicate live name
// (case-insensitive), partial PATCH semantics, and 404s for unknown ids.
func TestAdminProductRegistryLiveDB(t *testing.T) {
	h, pool := v1LiveDB(t)
	routes, _ := v1Routers(h)
	admin := cliSeedUser(t, pool, true)
	jwt := cliMintJWT(t, pool, admin)

	name := "Acme " + uuid.NewString()
	created := apCreate(t, routes, jwt, "  "+name+"  ", "Syncs tickets.")
	if created.Name != name || created.Description != "Syncs tickets." {
		t.Errorf("created = %+v, want trimmed name %q and the description", created, name)
	}
	if !created.Enabled || created.DeletedAt != nil || created.ActiveTokenCount != 0 {
		t.Errorf("created = %+v, want enabled, not deleted, 0 active tokens", created)
	}
	var createdBy uuid.UUID
	if err := pool.QueryRow(t.Context(), `SELECT created_by FROM products WHERE id = $1`, created.ID).Scan(&createdBy); err != nil {
		t.Fatalf("read created_by: %v", err)
	}
	if createdBy != admin {
		t.Errorf("created_by = %s, want the session's admin %s (never the body)", createdBy, admin)
	}

	listed, ok := apListProducts(t, routes, jwt)[created.ID]
	if !ok {
		t.Fatalf("GET /api/admin/products does not list the product just created (%s)", created.ID)
	}
	if listed.Name != name || !listed.Enabled || listed.DeletedAt != nil || listed.ActiveTokenCount != 0 {
		t.Errorf("listed = %+v", listed)
	}

	t.Run("duplicate live name is 409, case-insensitively", func(t *testing.T) {
		for _, dup := range []string{name, strings.ToUpper(name)} {
			body, _ := json.Marshal(map[string]string{"name": dup})
			rec := cookieReq(t, routes, http.MethodPost, "/api/admin/products", jwt, string(body))
			if rec.Code != http.StatusConflict {
				t.Errorf("POST duplicate %q = %d %q, want 409", dup, rec.Code, rec.Body.String())
			}
		}
	})

	t.Run("invalid display strings are 400 and store nothing", func(t *testing.T) {
		bad := "Evil\u202E" + uuid.NewString()
		body, _ := json.Marshal(map[string]string{"name": bad})
		if rec := cookieReq(t, routes, http.MethodPost, "/api/admin/products", jwt, string(body)); rec.Code != http.StatusBadRequest {
			t.Errorf("POST bidi name = %d %q, want 400", rec.Code, rec.Body.String())
		}
		var n int
		if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM products WHERE name = $1`, bad).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		if n != 0 {
			t.Errorf("a refused name was stored (%d rows)", n)
		}
	})

	t.Run("PATCH changes only the fields it names", func(t *testing.T) {
		rec := cookieReq(t, routes, http.MethodPatch, "/api/admin/products/"+created.ID, jwt, `{"description":"New text"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("PATCH description = %d %q", rec.Code, rec.Body.String())
		}
		if p := apDecodeProduct(t, rec.Code, rec.Body.String()).Product; p.Description != "New text" || !p.Enabled || p.Name != name {
			t.Errorf("after description PATCH: %+v, want description changed, still enabled, name unchanged", p)
		}
		rec = cookieReq(t, routes, http.MethodPatch, "/api/admin/products/"+created.ID, jwt, `{"enabled":false}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("PATCH enabled = %d %q", rec.Code, rec.Body.String())
		}
		if p := apDecodeProduct(t, rec.Code, rec.Body.String()).Product; p.Description != "New text" || p.Enabled {
			t.Errorf("after enabled PATCH: %+v, want disabled with the description kept", p)
		}
	})

	t.Run("unknown ids are 404", func(t *testing.T) {
		unknown := uuid.NewString()
		for _, c := range []struct{ method, path, body string }{
			{http.MethodPatch, "/api/admin/products/" + unknown, `{"enabled":true}`},
			{http.MethodDelete, "/api/admin/products/" + unknown, ""},
			{http.MethodPost, "/api/admin/product-tokens/" + unknown + "/revoke", ""},
		} {
			if rec := cookieReq(t, routes, c.method, c.path, jwt, c.body); rec.Code != http.StatusNotFound {
				t.Errorf("%s %s = %d %q, want 404", c.method, c.path, rec.Code, rec.Body.String())
			}
		}
	})
}

// TestAdminProductRoutesAuthLiveDB: a uza_ token reads both admin lists but is refused
// (401, cookie-only) on every write, and nothing changes; a non-admin session is 403
// on reads and writes; an admin's user-scope uzc_ is 403 on the reads.
func TestAdminProductRoutesAuthLiveDB(t *testing.T) {
	h, pool := v1LiveDB(t)
	routes, _ := v1Routers(h)
	admin := cliSeedUser(t, pool, true)
	member := cliSeedUser(t, pool, false)
	adminJWT := cliMintJWT(t, pool, admin)
	memberJWT := cliMintJWT(t, pool, member)
	uza := cliMintToken(t, pool, admin, clitoken.ScopeAdminRO)
	uzc := cliMintToken(t, pool, admin, clitoken.ScopeUser)

	p := apCreate(t, routes, adminJWT, "Auth "+uuid.NewString(), "")
	pt := v1MintProductToken(t, h.q, member, uuid.MustParse(p.ID), producttoken.Scopes, nil)

	for _, path := range []string{"/api/admin/products", "/api/admin/product-tokens"} {
		if rec := bearerReq(routes, http.MethodGet, path, uza); rec.Code != http.StatusOK {
			t.Errorf("uza_ GET %s = %d %q, want 200", path, rec.Code, rec.Body.String())
		}
		if rec := bearerReq(routes, http.MethodGet, path, uzc); rec.Code != http.StatusForbidden {
			t.Errorf("admin's uzc_ GET %s = %d %q, want 403 (masked to non-admin)", path, rec.Code, rec.Body.String())
		}
		if rec := cookieReq(t, routes, http.MethodGet, path, memberJWT, ""); rec.Code != http.StatusForbidden {
			t.Errorf("non-admin session GET %s = %d %q, want 403", path, rec.Code, rec.Body.String())
		}
	}

	writes := []struct{ method, path, body string }{
		{http.MethodPost, "/api/admin/products", `{"name":"via-bearer-` + uuid.NewString() + `"}`},
		{http.MethodPatch, "/api/admin/products/" + p.ID, `{"enabled":false}`},
		{http.MethodDelete, "/api/admin/products/" + p.ID, ""},
		{http.MethodPost, "/api/admin/product-tokens/" + pt.tokenID.String() + "/revoke", ""},
	}
	for _, wr := range writes {
		if rec := bearerReqBody(routes, wr.method, wr.path, uza, wr.body); rec.Code != http.StatusUnauthorized {
			t.Errorf("uza_ %s %s = %d %q, want 401 (admin writes are cookie-only)", wr.method, wr.path, rec.Code, rec.Body.String())
		}
		if rec := cookieReq(t, routes, wr.method, wr.path, memberJWT, wr.body); rec.Code != http.StatusForbidden {
			t.Errorf("non-admin session %s %s = %d %q, want 403", wr.method, wr.path, rec.Code, rec.Body.String())
		}
	}

	// Nothing the refused writes aimed at changed: the product is live and enabled and
	// the token still authenticates.
	got := apListProducts(t, routes, adminJWT)[p.ID]
	if !got.Enabled || got.DeletedAt != nil {
		t.Errorf("after refused writes the product is %+v, want live and enabled", got)
	}
	if st := apWhoami(t, routes, pt.token); st != http.StatusOK {
		t.Errorf("after refused writes whoami = %d, want 200 (the token must be untouched)", st)
	}
}

// TestAdminProductActionsKillTokenOnNextWhoamiLiveDB is the M4 done-criterion.
func TestAdminProductActionsKillTokenOnNextWhoamiLiveDB(t *testing.T) {
	h, pool := v1LiveDB(t)
	routes, _ := v1Routers(h)
	admin := cliSeedUser(t, pool, true)
	jwt := cliMintJWT(t, pool, admin)

	// seed registers a fresh product through the admin API and mints one token for a
	// fresh, ordinary user through the store queries; whoami must accept it first, so
	// every later 401 is caused by the admin action under test.
	seed := func(t *testing.T) (apitypes.ProductDTO, uuid.UUID, v1Product) {
		t.Helper()
		p := apCreate(t, routes, jwt, "Kill "+uuid.NewString(), "")
		owner := cliSeedUser(t, pool, false)
		pt := v1MintProductToken(t, h.q, owner, uuid.MustParse(p.ID), producttoken.Scopes, nil)
		if st := apWhoami(t, routes, pt.token); st != http.StatusOK {
			t.Fatalf("before the action whoami = %d, want 200", st)
		}
		return p, owner, pt
	}

	t.Run("disabling the product", func(t *testing.T) {
		p, _, pt := seed(t)
		rec := cookieReq(t, routes, http.MethodPatch, "/api/admin/products/"+p.ID, jwt, `{"enabled":false}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("PATCH enabled=false = %d %q", rec.Code, rec.Body.String())
		}
		if got := apDecodeProduct(t, rec.Code, rec.Body.String()).Product; got.Enabled || got.ActiveTokenCount != 1 {
			t.Errorf("PATCH response %+v, want disabled with active_token_count 1 (disable does not revoke)", got)
		}
		if st := apWhoami(t, routes, pt.token); st != http.StatusUnauthorized {
			t.Fatalf("after disable whoami = %d, want 401", st)
		}
		// Causality: re-enabling a LIVE product restores the same token.
		if rec := cookieReq(t, routes, http.MethodPatch, "/api/admin/products/"+p.ID, jwt, `{"enabled":true}`); rec.Code != http.StatusOK {
			t.Fatalf("PATCH enabled=true = %d %q", rec.Code, rec.Body.String())
		}
		if st := apWhoami(t, routes, pt.token); st != http.StatusOK {
			t.Errorf("after re-enable whoami = %d, want 200", st)
		}
	})

	t.Run("soft-deleting the product", func(t *testing.T) {
		p, owner, pt := seed(t)
		pid := uuid.MustParse(p.ID)
		// Two more rows that must NOT count as stopped: already revoked, already expired.
		revoked := v1MintProductToken(t, h.q, owner, pid, producttoken.Scopes, nil)
		cliMustExec(t, pool, `UPDATE product_tokens SET revoked = true WHERE id = $1`, revoked.tokenID)
		past := time.Now().Add(-time.Hour)
		expired := v1MintProductToken(t, h.q, owner, pid, producttoken.Scopes, &past)

		if got := apListProducts(t, routes, jwt)[p.ID]; got.ActiveTokenCount != 1 {
			t.Errorf("before delete active_token_count = %d, want 1 (what the UI confirm shows)", got.ActiveTokenCount)
		}

		rec := cookieReq(t, routes, http.MethodDelete, "/api/admin/products/"+p.ID, jwt, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("DELETE = %d %q, want 200", rec.Code, rec.Body.String())
		}
		resp := apDecodeProduct(t, rec.Code, rec.Body.String())
		if resp.StoppedTokenCount == nil || *resp.StoppedTokenCount != 1 {
			t.Errorf("stopped_token_count = %v, want 1 (the one live token, not the revoked or expired one)", resp.StoppedTokenCount)
		}
		if resp.Product.Enabled || resp.Product.DeletedAt == nil {
			t.Errorf("DELETE response product %+v, want disabled with deleted_at set", resp.Product)
		}
		if st := apWhoami(t, routes, pt.token); st != http.StatusUnauthorized {
			t.Fatalf("after delete whoami = %d, want 401", st)
		}

		// A deleted product cannot be re-enabled or edited, and deleting it again is a
		// 409 (it exists and is listed, so not a 404).
		for _, c := range []struct{ method, body string }{
			{http.MethodPatch, `{"enabled":true}`},
			{http.MethodPatch, `{"description":"back"}`},
			{http.MethodDelete, ""},
		} {
			if rec := cookieReq(t, routes, c.method, "/api/admin/products/"+p.ID, jwt, c.body); rec.Code != http.StatusConflict {
				t.Errorf("%s %s on a deleted product = %d %q, want 409", c.method, c.body, rec.Code, rec.Body.String())
			}
		}
		var enabled bool
		if err := pool.QueryRow(t.Context(), `SELECT enabled FROM products WHERE id = $1`, pid).Scan(&enabled); err != nil {
			t.Fatalf("read enabled: %v", err)
		}
		if enabled {
			t.Error("a deleted product was re-enabled")
		}
		if st := apWhoami(t, routes, pt.token); st != http.StatusUnauthorized {
			t.Errorf("after the refused re-enable whoami = %d, want 401", st)
		}

		// Still listed, with the audit trail: the product and every one of its token rows.
		got, ok := apListProducts(t, routes, jwt)[p.ID]
		if !ok || got.DeletedAt == nil || got.Enabled {
			t.Errorf("GET /api/admin/products after delete: listed=%t %+v, want listed, deleted, disabled", ok, got)
		}
		raw, httpRows, truncated := apListProductTokens(t, routes, jwt)
		rows := apInventoryRows(t, h.q, httpRows, truncated)
		for _, want := range []struct {
			tok     v1Product
			revoked bool
		}{{pt, false}, {revoked, true}, {expired, false}} {
			row, ok := rows[want.tok.tokenID.String()]
			if !ok {
				t.Errorf("token %s of the deleted product is missing from GET /api/admin/product-tokens", want.tok.tokenID)
				continue
			}
			if row.ProductID != p.ID || row.ProductName != p.Name || row.Revoked != want.revoked {
				t.Errorf("row %+v, want product %s %q revoked=%t", row, p.ID, p.Name, want.revoked)
			}
			if row.UserID != owner.String() || row.OwnerEmail != fmt.Sprintf("cli-%s@e2e", owner) {
				t.Errorf("row owner = %s %q, want %s cli-%s@e2e (the users JOIN)", row.UserID, row.OwnerEmail, owner, owner)
			}
			if row.TokenPrefix == "" || len(row.Scopes) == 0 {
				t.Errorf("row %+v lacks token_prefix or scopes", row)
			}
			// Metadata only: neither the value nor its sha256 (as a []byte token_hash
			// would marshal) is anywhere in the body.
			if strings.Contains(raw, want.tok.token) {
				t.Errorf("GET /api/admin/product-tokens carries a PLAINTEXT product token")
			}
			if strings.Contains(raw, base64.StdEncoding.EncodeToString(producttoken.Hash(want.tok.token))) {
				t.Errorf("GET /api/admin/product-tokens carries a product token's sha256")
			}
		}
		if strings.Contains(raw, "token_hash") {
			t.Error("GET /api/admin/product-tokens carries a token_hash key")
		}
		if row := rows[expired.tokenID.String()]; row.ExpiresAt == nil {
			t.Error("the expired row reports a null expires_at")
		}
	})

	t.Run("admin-revoking one token", func(t *testing.T) {
		p, owner, pt := seed(t)
		sibling := v1MintProductToken(t, h.q, owner, uuid.MustParse(p.ID), producttoken.Scopes, nil)
		path := "/api/admin/product-tokens/" + pt.tokenID.String() + "/revoke"
		if rec := cookieReq(t, routes, http.MethodPost, path, jwt, ""); rec.Code != http.StatusNoContent {
			t.Fatalf("POST revoke = %d %q, want 204", rec.Code, rec.Body.String())
		}
		if st := apWhoami(t, routes, pt.token); st != http.StatusUnauthorized {
			t.Fatalf("after admin revoke whoami = %d, want 401", st)
		}
		// One token, not the product: the sibling token still works.
		if st := apWhoami(t, routes, sibling.token); st != http.StatusOK {
			t.Errorf("the sibling token's whoami = %d, want 200 (revoke is one token)", st)
		}
		if rec := cookieReq(t, routes, http.MethodPost, path, jwt, ""); rec.Code != http.StatusNotFound {
			t.Errorf("second revoke = %d %q, want 404 (already revoked)", rec.Code, rec.Body.String())
		}
		_, httpRows, truncated := apListProductTokens(t, routes, jwt)
		rows := apInventoryRows(t, h.q, httpRows, truncated)
		if row, ok := rows[pt.tokenID.String()]; !ok || !row.Revoked {
			t.Errorf("inventory row after revoke: listed=%t revoked=%t, want listed and revoked", ok, row.Revoked)
		}
	})
}

// TestAdminDeleteDisabledProductStopsNothingLiveDB pins AdminDeleteProduct's
// stopped_token_count for a product that was ALREADY DISABLED: its tokens were refused
// since the disable, so the delete stopped none of them (0), while the product's own
// active_token_count keeps its registry meaning (the N un-revoked, unexpired rows). An
// unconditional stopped = active reports N here.
func TestAdminDeleteDisabledProductStopsNothingLiveDB(t *testing.T) {
	h, pool := v1LiveDB(t)
	routes, _ := v1Routers(h)
	admin := cliSeedUser(t, pool, true)
	jwt := cliMintJWT(t, pool, admin)

	p := apCreate(t, routes, jwt, "Disabled "+uuid.NewString(), "")
	owner := cliSeedUser(t, pool, false)
	const n = 3
	for range n {
		v1MintProductToken(t, h.q, owner, uuid.MustParse(p.ID), producttoken.Scopes, nil)
	}
	if rec := cookieReq(t, routes, http.MethodPatch, "/api/admin/products/"+p.ID, jwt, `{"enabled":false}`); rec.Code != http.StatusOK {
		t.Fatalf("PATCH enabled=false = %d %q", rec.Code, rec.Body.String())
	}

	rec := cookieReq(t, routes, http.MethodDelete, "/api/admin/products/"+p.ID, jwt, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE = %d %q, want 200", rec.Code, rec.Body.String())
	}
	resp := apDecodeProduct(t, rec.Code, rec.Body.String())
	if resp.StoppedTokenCount == nil {
		t.Fatalf("DELETE response has no stopped_token_count: %q", rec.Body.String())
	}
	if *resp.StoppedTokenCount != 0 {
		t.Errorf("stopped_token_count = %d, want 0 (the product was already disabled, so the delete stopped none of its %d tokens)", *resp.StoppedTokenCount, n)
	}
	if resp.Product.ActiveTokenCount != n {
		t.Errorf("product.active_token_count = %d, want %d (active rows, whatever the product's state)", resp.Product.ActiveTokenCount, n)
	}
	if resp.Product.Enabled || resp.Product.DeletedAt == nil {
		t.Errorf("DELETE response product %+v, want disabled with deleted_at set", resp.Product)
	}
}

// TestAdminListProductTokensTruncatedLiveDB pins the inventory's "truncated" flag: false
// (and present) whenever the table fits the bound, and true with exactly bound rows,
// all of them active, once it does not (bound lowered through the test override so a
// handful of rows reach it).
func TestAdminListProductTokensTruncatedLiveDB(t *testing.T) {
	h, pool := v1LiveDB(t)
	routes, _ := v1Routers(h)
	admin := cliSeedUser(t, pool, true)
	jwt := cliMintJWT(t, pool, admin)
	owner := cliSeedUser(t, pool, false)
	pid := v1SeedProduct(t, h.q, owner)
	for range 3 {
		v1MintProductToken(t, h.q, owner, pid, producttoken.Scopes, nil)
	}

	t.Run("default bound", func(t *testing.T) {
		// The table is shared, so the expectation is derived from its size; -p 1 keeps
		// other packages from writing between the count and the request.
		var total int64
		if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM product_tokens`).Scan(&total); err != nil {
			t.Fatalf("count: %v", err)
		}
		_, rows, truncated := apListProductTokens(t, routes, jwt)
		if want := total > maxAdminProductTokenRows; truncated != want {
			t.Errorf("truncated = %t with %d rows in the table, want %t (bound %d)", truncated, total, want, maxAdminProductTokenRows)
		}
		if want := min(total, maxAdminProductTokenRows); int64(len(rows)) != want {
			t.Errorf("listed %d rows with %d in the table, want %d", len(rows), total, want)
		}
	})

	t.Run("lowered bound", func(t *testing.T) {
		h.adminProductTokenRowsOverride = 2
		t.Cleanup(func() { h.adminProductTokenRowsOverride = 0 })
		_, rows, truncated := apListProductTokens(t, routes, jwt)
		if !truncated || len(rows) != 2 {
			t.Fatalf("with bound 2 over at least 3 tokens: %d rows, truncated=%t; want 2 rows, truncated", len(rows), truncated)
		}
		for _, r := range rows {
			if r.Revoked || (r.ExpiresAt != nil && !r.ExpiresAt.After(time.Now())) {
				t.Errorf("row %+v survived the cut, want only active rows (active first)", r)
			}
		}
	})
}
