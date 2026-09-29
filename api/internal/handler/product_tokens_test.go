package handler

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/producttoken"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1907 M5: validateMintProductToken is the DB-free half of the mint. These pin its
// scope normalisation (deduped, in producttoken.Scopes order), the D10 expiry enum and
// its 90d default, and the D11 name rule.

func validMintReq() mintProductTokenRequest {
	return mintProductTokenRequest{
		ProductID: uuid.NewString(),
		Name:      "ci runner",
		Scopes:    []string{producttoken.ScopeJobsRead},
	}
}

func TestValidateMintProductTokenScopes(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"single", []string{"jobs:read"}, []string{"jobs:read"}},
		{"reordered to display order", []string{"jobs:read", "jobs:run"}, []string{"jobs:run", "jobs:read"}},
		{"duplicates dropped", []string{"jobs:run", "jobs:run", "jobs:read", "jobs:read"}, []string{"jobs:run", "jobs:read"}},
		{"duplicate of one", []string{"jobs:read", "jobs:read"}, []string{"jobs:read"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := validMintReq()
			req.Scopes = c.in
			v, err := validateMintProductToken(req)
			if err != nil {
				t.Fatalf("validate(%v) = %v, want ok", c.in, err)
			}
			if !slices.Equal(v.scopes, c.want) {
				t.Errorf("scopes(%v) = %v, want %v", c.in, v.scopes, c.want)
			}
		})
	}
}

func TestValidateMintProductTokenRefusals(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*mintProductTokenRequest)
	}{
		{"nil scopes", func(r *mintProductTokenRequest) { r.Scopes = nil }},
		{"empty scopes", func(r *mintProductTokenRequest) { r.Scopes = []string{} }},
		{"unknown scope", func(r *mintProductTokenRequest) { r.Scopes = []string{"jobs:read", "jobs:admin"} }},
		{"empty-string scope", func(r *mintProductTokenRequest) { r.Scopes = []string{""} }},
		{"case-variant scope", func(r *mintProductTokenRequest) { r.Scopes = []string{"JOBS:READ"} }},
		{"unknown expiry", func(r *mintProductTokenRequest) { r.Expiry = "7d" }},
		{"timestamp expiry", func(r *mintProductTokenRequest) { r.Expiry = "2030-01-01T00:00:00Z" }},
		{"case-variant expiry", func(r *mintProductTokenRequest) { r.Expiry = "Never" }},
		{"bad product id", func(r *mintProductTokenRequest) { r.ProductID = "not-a-uuid" }},
		{"empty product id", func(r *mintProductTokenRequest) { r.ProductID = "" }},
		{"empty name", func(r *mintProductTokenRequest) { r.Name = "" }},
		{"blank name", func(r *mintProductTokenRequest) { r.Name = "   " }},
		{"overlong name", func(r *mintProductTokenRequest) { r.Name = strings.Repeat("a", maxProductNameBytes+1) }},
		{"control char in name", func(r *mintProductTokenRequest) { r.Name = "ci\nrunner" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := validMintReq()
			c.mutate(&req)
			if v, err := validateMintProductToken(req); err == nil {
				t.Fatalf("validate(%+v) = %+v, want an error", req, v)
			}
		})
	}
}

func TestValidateMintProductTokenExpiry(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	cases := []struct {
		expiry string
		want   time.Duration // 0 = never
	}{
		{"", 90 * day}, // D10: the default is bounded, 90 days, never "never"
		{"30d", 30 * day},
		{"90d", 90 * day},
		{"1y", 365 * day},
		{"never", 0},
	}
	for _, c := range cases {
		t.Run("expiry="+c.expiry, func(t *testing.T) {
			req := validMintReq()
			req.Expiry = c.expiry
			v, err := validateMintProductToken(req)
			if err != nil {
				t.Fatalf("validate(expiry=%q) = %v", c.expiry, err)
			}
			got := v.expiresAt(now)
			if c.want == 0 {
				if got.Valid {
					t.Errorf("expiry %q: expires_at = %v, want NULL (never)", c.expiry, got.Time)
				}
				return
			}
			if !got.Valid || !got.Time.Equal(now.Add(c.want)) {
				t.Errorf("expiry %q: expires_at = %+v, want %v", c.expiry, got, now.Add(c.want))
			}
		})
	}
}

func TestValidateMintProductTokenNameTrimmed(t *testing.T) {
	req := validMintReq()
	req.Name = "  ci runner  "
	v, err := validateMintProductToken(req)
	if err != nil {
		t.Fatal(err)
	}
	if v.name != "ci runner" {
		t.Errorf("name = %q, want trimmed %q", v.name, "ci runner")
	}
	if v.productID.String() != req.ProductID {
		t.Errorf("productID = %s, want %s", v.productID, req.ProductID)
	}
}

// TestMintProductTokenRefusesBeforeTheDatabase: an unknown JSON field (a client trying
// to set its own expires_at, D10) and an invalid body are 400 before any query runs. The
// Handler has no store and no pool, so reaching the database would panic.
func TestMintProductTokenRefusesBeforeTheDatabase(t *testing.T) {
	h := &Handler{}
	pid := uuid.NewString()
	cases := map[string]string{
		"unknown expires_at field": `{"product_id":"` + pid + `","name":"n","scopes":["jobs:read"],"expires_at":"2099-01-01T00:00:00Z"}`,
		"unknown field":            `{"product_id":"` + pid + `","name":"n","scopes":["jobs:read"],"user_id":"x"}`,
		"not json":                 `nope`,
		"unknown scope":            `{"product_id":"` + pid + `","name":"n","scopes":["jobs:admin"]}`,
		"unknown expiry":           `{"product_id":"` + pid + `","name":"n","scopes":["jobs:read"],"expiry":"5y"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/me/product-tokens/", strings.NewReader(body))
			req = req.WithContext(mw.ContextWithUser(req.Context(), store.User{ID: uuid.New()}))
			rec := httptest.NewRecorder()
			h.MintProductToken(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d %q, want 400", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestProductTokenListsAskForBoundPlusOne pins both product-token list bounds without a
// database: each handler asks its store query for its named constant PLUS ONE rows (the
// extra row is how it detects a cut), and an uncut result is served as
// {"tokens": [], "truncated": false}. The fake DB captures the Query args and returns
// no rows.
func TestProductTokenListsAskForBoundPlusOne(t *testing.T) {
	if maxMyProductTokenRows != 200 || maxAdminProductTokenRows != 1000 {
		t.Fatalf("bounds moved: my=%d admin=%d; PRD #1907's audit fixed them at 200 and 1000",
			maxMyProductTokenRows, maxAdminProductTokenRows)
	}
	userID := uuid.New()
	for _, c := range []struct {
		name     string
		serve    func(h *Handler) http.HandlerFunc
		wantArgs []any
	}{
		{"GET /api/me/product-tokens", func(h *Handler) http.HandlerFunc { return h.ListMyProductTokens },
			[]any{userID, int32(maxMyProductTokenRows + 1)}},
		{"GET /api/admin/product-tokens", func(h *Handler) http.HandlerFunc { return h.AdminListProductTokens },
			[]any{int32(maxAdminProductTokenRows + 1)}},
	} {
		t.Run(c.name, func(t *testing.T) {
			db := &fakeViewerListDB{}
			h := &Handler{q: store.New(db)}
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req = req.WithContext(mw.ContextWithUser(req.Context(), store.User{ID: userID}))
			rec := httptest.NewRecorder()
			c.serve(h)(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d %q, want 200", rec.Code, rec.Body.String())
			}
			if !db.called || !slices.Equal(db.gotArgs, c.wantArgs) {
				t.Errorf("store query args = %#v, want %#v (the bound plus one)", db.gotArgs, c.wantArgs)
			}
			if got := strings.TrimSpace(rec.Body.String()); got != `{"tokens":[],"truncated":false}` {
				t.Errorf("body = %s, want {\"tokens\":[],\"truncated\":false}", got)
			}
		})
	}
}

// TestCutProductTokenList: the bound+1 fetch is cut to bound and flagged only when the
// extra row came back; a result at or under the bound is served whole.
func TestCutProductTokenList(t *testing.T) {
	for _, c := range []struct {
		n, wantLen int
		bound      int32
		wantCut    bool
	}{{0, 0, 2, false}, {2, 2, 2, false}, {3, 2, 2, true}} {
		rows, cut := cutProductTokenList(make([]int, c.n), c.bound)
		if len(rows) != c.wantLen || cut != c.wantCut {
			t.Errorf("cut(%d rows, bound %d) = %d rows, %t; want %d, %t", c.n, c.bound, len(rows), cut, c.wantLen, c.wantCut)
		}
	}
	if productTokenListBound(0, 200) != 200 || productTokenListBound(5, 200) != 5 {
		t.Error("productTokenListBound must use the override only when it is positive")
	}
}
