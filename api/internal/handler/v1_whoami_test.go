package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/config"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/producttoken"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// nilScopesV1Store is a V1CallerStore whose one product token row carries a NIL
// scopes slice. The column is NOT NULL and non-empty, so a live row never looks like
// this; the fake is how the handler's own normalisation is reached at all.
type nilScopesV1Store struct {
	hash      []byte
	user      store.User
	productID uuid.UUID
}

func (s nilScopesV1Store) GetProductTokenForAuth(_ context.Context, h []byte) (store.GetProductTokenForAuthRow, error) {
	if string(h) != string(s.hash) {
		return store.GetProductTokenForAuthRow{}, pgx.ErrNoRows
	}
	return store.GetProductTokenForAuthRow{
		ID: uuid.New(), UserID: s.user.ID, ProductID: s.productID, Scopes: nil, ProductName: "nil-scopes product",
	}, nil
}

func (nilScopesV1Store) GetCLITokenByHash(context.Context, []byte) (store.CliToken, error) {
	return store.CliToken{}, pgx.ErrNoRows
}

func (s nilScopesV1Store) GetUserByID(_ context.Context, id uuid.UUID) (store.User, error) {
	if id != s.user.ID {
		return store.User{}, pgx.ErrNoRows
	}
	return s.user, nil
}

func (nilScopesV1Store) TouchProductToken(context.Context, store.TouchProductTokenParams) error {
	return nil
}

func (nilScopesV1Store) TouchCLIToken(context.Context, store.TouchCLITokenParams) error {
	return nil
}

// TestV1WhoamiNilScopesEncodeAsEmptyArray drives V1Whoami behind the real
// RequireV1Caller with a principal whose Scopes is nil, and asserts the wire says
// "scopes":[] rather than null: the OpenAPI document declares scopes a non-nullable
// array, and V1Whoami's normalisation is the only thing that makes that true for a
// nil slice (encoding/json writes a nil slice as null).
func TestV1WhoamiNilScopesEncodeAsEmptyArray(t *testing.T) {
	tok, hash, _, err := producttoken.Generate()
	if err != nil {
		t.Fatalf("producttoken.Generate: %v", err)
	}
	st := nilScopesV1Store{
		hash:      hash,
		user:      store.User{ID: uuid.New(), IsActive: true},
		productID: uuid.New(),
	}
	h := &Handler{}
	srv := mw.RequireV1Caller(st, config.Config{})(http.HandlerFunc(h.V1Whoami))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/whoami", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d %q, want 200", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"scopes":[]`) {
		t.Fatalf("body %q lacks \"scopes\":[] (a nil scopes slice must not reach the wire as null)", body)
	}
	if strings.Contains(body, `"scopes":null`) {
		t.Fatalf("body %q carries \"scopes\":null", body)
	}
	got := v1DecodeWhoami(t, body)
	if got.Scopes == nil || len(got.Scopes) != 0 {
		t.Fatalf("decoded scopes = %#v, want an empty non-nil slice", got.Scopes)
	}
	if got.Product == nil || got.Product.ID != st.productID.String() {
		t.Fatalf("product = %+v, want id %s (the principal really was a uzp_ caller)", got.Product, st.productID)
	}
}
