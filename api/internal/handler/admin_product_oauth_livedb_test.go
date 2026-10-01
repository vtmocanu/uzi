package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/oauthsrv"
)

// PRD #1910 M1: the admin writes that make a product an OAuth client, through the
// PRODUCTION router (h.Routes via v1Routers). Skipped unless UZI_TEST_DATABASE_URL points
// at a throwaway Postgres.

func oauthPut(t *testing.T, routes http.Handler, jwt, id string, uris, scopes any) (int, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"redirect_uris": uris, "scopes": scopes})
	rec := cookieReq(t, routes, http.MethodPut, "/api/admin/products/"+id+"/oauth", jwt, string(body))
	return rec.Code, rec.Body.String()
}

func TestAdminProductOAuthClientLiveDB(t *testing.T) {
	h, pool := v1LiveDB(t)
	routes, _ := v1Routers(h)
	admin := cliSeedUser(t, pool, true)
	jwt := cliMintJWT(t, pool, admin)
	p := apCreate(t, routes, jwt, "OAuth "+uuid.NewString(), "")

	if p.OAuthClient.IsClient || p.OAuthClient.HasSecret || len(p.OAuthClient.RedirectURIs) != 0 ||
		p.OAuthClient.RedirectURIs == nil || p.OAuthClient.Scopes == nil || p.OAuthClient.RotatedAt != nil {
		t.Fatalf("a new product is not a client, with empty non-null lists: %+v", p.OAuthClient)
	}

	uris := []string{"https://app.example.com/oauth/callback", "http://127.0.0.1:8123/cb"}
	code, body := oauthPut(t, routes, jwt, p.ID, uris, []string{"jobs:run"})
	if code != http.StatusOK {
		t.Fatalf("PUT oauth = %d %q, want 200", code, body)
	}
	got := apDecodeProduct(t, code, body).Product.OAuthClient
	if len(got.RedirectURIs) != 2 || got.RedirectURIs[0] != uris[0] || got.RedirectURIs[1] != uris[1] ||
		len(got.Scopes) != 1 || got.Scopes[0] != "jobs:run" {
		t.Fatalf("after PUT: %+v", got)
	}
	if got.HasSecret || got.IsClient {
		t.Fatalf("no secret yet, so not a client: %+v", got)
	}

	// Rotate: the plaintext is in this response only.
	rec := cookieReq(t, routes, http.MethodPost, "/api/admin/products/"+p.ID+"/oauth/secret", jwt, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("rotate = %d %q, want 200", rec.Code, rec.Body.String())
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store on the secret response", cc)
	}
	var rot apitypes.RotateProductClientSecretResponse
	dec := json.NewDecoder(bytes.NewReader(rec.Body.Bytes()))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rot); err != nil {
		t.Fatalf("decode rotate response: %v", err)
	}
	if !strings.HasPrefix(rot.ClientSecret, oauthsrv.SecretPrefix) || len(rot.ClientSecret) < 40 {
		t.Fatalf("client_secret = %q, want a uzs_ secret", rot.ClientSecret)
	}
	oc := rot.Product.OAuthClient
	if !oc.HasSecret || !oc.IsClient || oc.RotatedAt == nil || oc.SecretPrefix != rot.ClientSecret[:8] {
		t.Fatalf("after rotate: %+v (secret prefix wanted %q)", oc, rot.ClientSecret[:8])
	}

	// Stored: only the sha256 and the display prefix, never the plaintext.
	var hash []byte
	var prefix string
	if err := pool.QueryRow(t.Context(),
		`SELECT client_secret_hash, client_secret_prefix FROM products WHERE id = $1`, p.ID).Scan(&hash, &prefix); err != nil {
		t.Fatalf("read secret columns: %v", err)
	}
	if !bytes.Equal(hash, oauthsrv.HashSecret(rot.ClientSecret)) || prefix != oc.SecretPrefix {
		t.Errorf("stored hash/prefix do not match the shown secret (prefix %q)", prefix)
	}

	// A later read shows has_secret and the prefix, and never the secret or the hash.
	lrec := cookieReq(t, routes, http.MethodGet, "/api/admin/products", jwt, "")
	if lrec.Code != http.StatusOK {
		t.Fatalf("list = %d", lrec.Code)
	}
	raw := lrec.Body.String()
	if strings.Contains(raw, rot.ClientSecret) || strings.Contains(raw, "client_secret") {
		t.Fatalf("the product list leaked the secret or a client_secret key")
	}
	listed := apListProducts(t, routes, jwt)[p.ID].OAuthClient
	if !listed.HasSecret || !listed.IsClient || listed.SecretPrefix != oc.SecretPrefix || listed.RotatedAt == nil ||
		len(listed.RedirectURIs) != 2 || len(listed.Scopes) != 1 {
		t.Fatalf("listed oauth_client = %+v", listed)
	}

	// Rotating again replaces the stored hash immediately.
	rec2 := cookieReq(t, routes, http.MethodPost, "/api/admin/products/"+p.ID+"/oauth/secret", jwt, "")
	var rot2 apitypes.RotateProductClientSecretResponse
	if err := json.Unmarshal(rec2.Body.Bytes(), &rot2); err != nil || rec2.Code != http.StatusOK {
		t.Fatalf("second rotate = %d %v", rec2.Code, err)
	}
	var hash2 []byte
	if err := pool.QueryRow(t.Context(), `SELECT client_secret_hash FROM products WHERE id = $1`, p.ID).Scan(&hash2); err != nil {
		t.Fatal(err)
	}
	if rot2.ClientSecret == rot.ClientSecret || bytes.Equal(hash2, hash) || !bytes.Equal(hash2, oauthsrv.HashSecret(rot2.ClientSecret)) {
		t.Error("the second rotation did not replace the stored secret")
	}

	// Clearing both lists stops the product being a client but keeps the secret.
	code, body = oauthPut(t, routes, jwt, p.ID, []string{}, []string{})
	if code != http.StatusOK {
		t.Fatalf("clear = %d %q", code, body)
	}
	cleared := apDecodeProduct(t, code, body).Product.OAuthClient
	if cleared.IsClient || !cleared.HasSecret || len(cleared.RedirectURIs) != 0 || cleared.RedirectURIs == nil {
		t.Errorf("after clearing: %+v, want not a client, secret kept, [] not null", cleared)
	}
}

func TestAdminProductOAuthValidationLiveDB(t *testing.T) {
	h, pool := v1LiveDB(t)
	routes, _ := v1Routers(h)
	admin := cliSeedUser(t, pool, true)
	jwt := cliMintJWT(t, pool, admin)
	p := apCreate(t, routes, jwt, "OAuthBad "+uuid.NewString(), "")

	ok := "https://app.example.com/cb"
	six := []string{}
	for _, s := range []string{"a", "b", "c", "d", "e", "f"} {
		six = append(six, "https://"+s+".example.com/cb")
	}
	cases := []struct {
		name   string
		uris   any
		scopes any
	}{
		{"localhost", []string{"http://localhost:8080/cb"}, []string{"jobs:run"}},
		{"fragment", []string{"https://app.example.com/cb#x"}, []string{"jobs:run"}},
		{"http non-loopback", []string{"http://app.example.com/cb"}, []string{"jobs:run"}},
		{"loopback without port", []string{"http://127.0.0.1/cb"}, []string{"jobs:run"}},
		{"userinfo", []string{"https://u:p@app.example.com/cb"}, []string{"jobs:run"}},
		{"sixth URI", six, []string{"jobs:run"}},
		{"duplicate", []string{ok, ok}, []string{"jobs:run"}},
		{"unknown scope", []string{ok}, []string{"jobs:run", "admin"}},
		{"duplicate scope", []string{ok}, []string{"jobs:run", "jobs:run"}},
		{"no scopes with URIs", []string{ok}, []string{}},
		{"scopes without URIs", []string{}, []string{"jobs:run"}},
		{"URIs not a list", "https://app.example.com/cb", []string{"jobs:run"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if code, body := oauthPut(t, routes, jwt, p.ID, tc.uris, tc.scopes); code != http.StatusBadRequest {
				t.Errorf("PUT = %d %q, want 400", code, body)
			}
		})
	}
	for _, body := range []string{`{}`, `{"redirect_uris":[]}`, `{"scopes":[]}`, `{"redirect_uris":null,"scopes":null}`, `{"redirect_uris":[],"scopes":[],"x":1}`} {
		if rec := cookieReq(t, routes, http.MethodPut, "/api/admin/products/"+p.ID+"/oauth", jwt, body); rec.Code != http.StatusBadRequest {
			t.Errorf("PUT %s = %d %q, want 400", body, rec.Code, rec.Body.String())
		}
	}
	// Every refusal left the registration untouched.
	if oc := apListProducts(t, routes, jwt)[p.ID].OAuthClient; len(oc.RedirectURIs) != 0 || len(oc.Scopes) != 0 {
		t.Errorf("a refused PUT stored something: %+v", oc)
	}
}

func TestAdminProductOAuthUnknownAndDeletedLiveDB(t *testing.T) {
	h, pool := v1LiveDB(t)
	routes, _ := v1Routers(h)
	admin := cliSeedUser(t, pool, true)
	jwt := cliMintJWT(t, pool, admin)

	unknown := uuid.NewString()
	if code, body := oauthPut(t, routes, jwt, unknown, []string{"https://app.example.com/cb"}, []string{"jobs:run"}); code != http.StatusNotFound {
		t.Errorf("PUT unknown = %d %q, want 404", code, body)
	}
	if rec := cookieReq(t, routes, http.MethodPost, "/api/admin/products/"+unknown+"/oauth/secret", jwt, ""); rec.Code != http.StatusNotFound {
		t.Errorf("rotate unknown = %d %q, want 404", rec.Code, rec.Body.String())
	}

	p := apCreate(t, routes, jwt, "OAuthDel "+uuid.NewString(), "")
	if rec := cookieReq(t, routes, http.MethodDelete, "/api/admin/products/"+p.ID, jwt, ""); rec.Code != http.StatusOK {
		t.Fatalf("delete = %d %q", rec.Code, rec.Body.String())
	}
	// A soft-deleted product is a 409 like PATCH (it still exists and is listed).
	if code, body := oauthPut(t, routes, jwt, p.ID, []string{"https://app.example.com/cb"}, []string{"jobs:run"}); code != http.StatusConflict {
		t.Errorf("PUT deleted = %d %q, want 409", code, body)
	}
	if rec := cookieReq(t, routes, http.MethodPost, "/api/admin/products/"+p.ID+"/oauth/secret", jwt, ""); rec.Code != http.StatusConflict {
		t.Errorf("rotate deleted = %d %q, want 409", rec.Code, rec.Body.String())
	}
	var has bool
	if err := pool.QueryRow(t.Context(), `SELECT client_secret_hash IS NOT NULL FROM products WHERE id = $1`, p.ID).Scan(&has); err != nil || has {
		t.Errorf("a refused rotate stored a secret (has=%v err=%v)", has, err)
	}
}

// A uza_ (or uzc_) Bearer is refused on both writes (cookie-only group), a non-admin
// session is 403, and nothing changes.
func TestAdminProductOAuthRoutesAuthLiveDB(t *testing.T) {
	h, pool := v1LiveDB(t)
	routes, _ := v1Routers(h)
	admin := cliSeedUser(t, pool, true)
	member := cliSeedUser(t, pool, false)
	jwt := cliMintJWT(t, pool, admin)
	memberJWT := cliMintJWT(t, pool, member)
	uza := cliMintToken(t, pool, admin, clitoken.ScopeAdminRO)
	uzc := cliMintToken(t, pool, admin, clitoken.ScopeUser)
	p := apCreate(t, routes, jwt, "OAuthAuth "+uuid.NewString(), "")

	writes := []struct{ method, path, body string }{
		{http.MethodPut, "/api/admin/products/" + p.ID + "/oauth", `{"redirect_uris":["https://app.example.com/cb"],"scopes":["jobs:run"]}`},
		{http.MethodPost, "/api/admin/products/" + p.ID + "/oauth/secret", ""},
	}
	for _, wr := range writes {
		for name, tok := range map[string]string{"uza_": uza, "uzc_": uzc} {
			if rec := bearerReqBody(routes, wr.method, wr.path, tok, wr.body); rec.Code != http.StatusUnauthorized {
				t.Errorf("%s %s %s = %d %q, want 401 (admin writes are cookie-only)", name, wr.method, wr.path, rec.Code, rec.Body.String())
			}
		}
		if rec := cookieReq(t, routes, wr.method, wr.path, memberJWT, wr.body); rec.Code != http.StatusForbidden {
			t.Errorf("non-admin %s %s = %d %q, want 403", wr.method, wr.path, rec.Code, rec.Body.String())
		}
	}
	oc := apListProducts(t, routes, jwt)[p.ID].OAuthClient
	if len(oc.RedirectURIs) != 0 || oc.HasSecret {
		t.Errorf("a refused write changed the product: %+v", oc)
	}
}
