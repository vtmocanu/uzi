package handler

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/producttoken"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// The user's own product tokens, Settings > Access (PRD #1907 M5).
//
// Routing (routes_me.go) is COOKIE-ONLY, RequireAuth with the session's CSRF check, for
// every route here (D14): minting a credential is a browser action, and a Bearer of any
// class (uzc_, uza_, uzp_) is refused before a handler here runs, so a stolen token can
// neither mint replacements nor list the user's other credentials.
//
//	GET    /api/me/product-tokens           the caller's tokens, revoked and expired ones
//	                                        included (as GET /api/me/cli-tokens), newest first
//	GET    /api/me/product-tokens/products  the mint picker: enabled, live products only,
//	                                        {id, name, description}
//	POST   /api/me/product-tokens           mint; per-user rate limited (authLimiter)
//	DELETE /api/me/product-tokens/{id}      revoke one of the caller's own tokens
//
// Status codes of the mint: 400 for an invalid body (unknown field, bad product_id,
// name, scopes or expiry); 404 for an unknown product id; 409 for a product that exists
// but is disabled or soft-deleted (it cannot be minted for), and 409 for the D15 cap
// (maxActiveProductTokensPerProduct active tokens for this user and product); 429 from
// the limiter; 201 with apitypes.MintProductTokenResponse, the only time the token
// value is ever returned.

// maxActiveProductTokensPerProduct is the D15 cap: at most this many ACTIVE (not
// revoked, not expired) product tokens per user per product.
const maxActiveProductTokensPerProduct = 10

// productTokenExpiryDefault is the expiry a mint that omits it gets (D10): bounded, not
// "never".
const productTokenExpiryDefault = "90d"

// productTokenLifetimes are the lifetimes the mint offers (D10). The client names one;
// the SERVER turns it into expires_at, so no client can set an arbitrary timestamp.
// "never" (a NULL expires_at) is the fourth choice and is not in this map.
var productTokenLifetimes = map[string]time.Duration{
	"30d": 30 * 24 * time.Hour,
	"90d": 90 * 24 * time.Hour,
	"1y":  365 * 24 * time.Hour,
}

// productTokenExpiryNever is the "never expires" choice.
const productTokenExpiryNever = "never"

// mintProductTokenRequest is the POST /api/me/product-tokens body. Unknown fields are a
// 400 (httpx.DecodeJSON), so a client cannot smuggle an expires_at timestamp.
type mintProductTokenRequest struct {
	ProductID string   `json:"product_id"`
	Name      string   `json:"name"`
	Scopes    []string `json:"scopes"`
	Expiry    string   `json:"expiry"`
}

// validMint is a validated mint request. never is true for the "never" expiry; else
// lifetime is the chosen duration.
type validMint struct {
	productID uuid.UUID
	name      string
	scopes    []string
	lifetime  time.Duration
	never     bool
}

// expiresAt is the expires_at the server stores for this mint, relative to now.
func (v validMint) expiresAt(now time.Time) pgtype.Timestamptz {
	if v.never {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: now.Add(v.lifetime), Valid: true}
}

// validateMintProductToken checks a mint body without touching the database:
//   - product_id is a UUID;
//   - name is trimmed, non-empty, at most maxCLITokenNameBytes (200) bytes and
//     termsafe-clean (D11: it is shown beside the owner's email in the admin
//     inventory), the same rule as a product name;
//   - scopes is non-empty and every element is a known scope; duplicates are dropped
//     and the result is in producttoken.Scopes order, so ["jobs:read","jobs:run"] and
//     ["jobs:run","jobs:run","jobs:read"] store the same set;
//   - expiry is one of 30d, 90d, 1y, never; empty means 90d (D10); anything else is
//     refused.
func validateMintProductToken(req mintProductTokenRequest) (validMint, error) {
	var v validMint
	pid, err := uuid.Parse(req.ProductID)
	if err != nil {
		return v, errors.New("product_id must be a product id")
	}
	v.productID = pid
	if v.name, err = validateProductName(req.Name); err != nil {
		return v, err
	}
	if len(req.Scopes) == 0 {
		return v, fmt.Errorf("scopes must name at least one of %v", producttoken.Scopes)
	}
	for _, s := range req.Scopes {
		if !producttoken.ValidScope(s) {
			return v, fmt.Errorf("unknown scope %q: scopes must be among %v", s, producttoken.Scopes)
		}
	}
	for _, s := range producttoken.Scopes {
		if slices.Contains(req.Scopes, s) {
			v.scopes = append(v.scopes, s)
		}
	}
	if !producttoken.ValidScopes(v.scopes) {
		// Unreachable after the loops above; kept so the stored set is always one the
		// package's own validator accepts.
		return v, errors.New("invalid scopes")
	}
	expiry := req.Expiry
	if expiry == "" {
		expiry = productTokenExpiryDefault
	}
	if expiry == productTokenExpiryNever {
		v.never = true
		return v, nil
	}
	lifetime, ok := productTokenLifetimes[expiry]
	if !ok {
		return v, errors.New(`expiry must be one of "30d", "90d", "1y" or "never"`)
	}
	v.lifetime = lifetime
	return v, nil
}

// Mint refusals, mapped to status codes by MintProductToken.
var (
	errMintProductUnavailable = errors.New("product is disabled or deleted")
	errMintProductTokenCap    = errors.New("product token cap reached")
)

func productTokenDTO(id, productID uuid.UUID, productName, name, prefix string, scopes []string,
	revoked bool, createdAt, lastUsedAt, expiresAt pgtype.Timestamptz, lastUsedIP fmt.Stringer,
) apitypes.ProductTokenDTO {
	dto := apitypes.ProductTokenDTO{
		ID:          id.String(),
		ProductID:   productID.String(),
		ProductName: productName,
		Name:        name,
		TokenPrefix: prefix,
		Scopes:      scopes,
		Revoked:     revoked,
		CreatedAt:   createdAt.Time,
		LastUsedAt:  timePtr(lastUsedAt.Valid, lastUsedAt.Time),
		ExpiresAt:   timePtr(expiresAt.Valid, expiresAt.Time),
	}
	// The column is NOT NULL and non-empty, but the DTO promises a non-null array.
	if dto.Scopes == nil {
		dto.Scopes = []string{}
	}
	if lastUsedIP != nil {
		s := lastUsedIP.String()
		dto.LastUsedIP = &s
	}
	return dto
}

// maxMyProductTokenRows bounds one GET /api/me/product-tokens response (PRD #1907 M5
// security audit, H1). Revoked rows are kept, so a user looping mint -> revoke would
// otherwise grow their own list without limit.
const maxMyProductTokenRows = 200

// productTokenListBound is a product-token list's row bound: the test override when it
// is positive, else the named constant def.
func productTokenListBound(override, def int32) int32 {
	if override > 0 {
		return override
	}
	return def
}

// cutProductTokenList trims rows, fetched with a LIMIT of bound+1, to bound and reports
// whether the extra row came back, i.e. whether the list was cut.
func cutProductTokenList[T any](rows []T, bound int32) ([]T, bool) {
	if len(rows) > int(bound) {
		return rows[:bound], true
	}
	return rows, false
}

// ListMyProductTokens returns the caller's product tokens, metadata only (the value is
// never stored). Revoked and expired tokens are included, exactly as the CLI-token list
// includes revoked ones: the list is where a user sees what was revoked and when a token
// was last used. Tokens of disabled or deleted products are included too; they still
// exist.
//
// BOUNDED: at most maxMyProductTokenRows (200) rows, ACTIVE tokens (not revoked, not
// expired) first, then newest first, so the cut drops revoked/expired history before
// any active token. The query is asked for one row more than the bound, so the response
// says whether anything was cut: {"tokens": [...], "truncated": bool}.
func (h *Handler) ListMyProductTokens(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	bound := productTokenListBound(h.myProductTokenRowsOverride, maxMyProductTokenRows)
	rows, err := h.q.ListProductTokensForUser(r.Context(), store.ListProductTokensForUserParams{
		UserID:  user.ID,
		MaxRows: bound + 1,
	})
	if err != nil {
		slog.Error("list product tokens", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	rows, truncated := cutProductTokenList(rows, bound)
	out := make([]apitypes.ProductTokenDTO, 0, len(rows))
	for _, t := range rows {
		var ip fmt.Stringer
		if t.LastUsedIp != nil {
			ip = t.LastUsedIp
		}
		out = append(out, productTokenDTO(t.ID, t.ProductID, t.ProductName, t.Name, t.TokenPrefix,
			t.Scopes, t.Revoked, t.CreatedAt, t.LastUsedAt, t.ExpiresAt, ip))
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"tokens": out, "truncated": truncated})
}

// ListMintableProducts is the mint picker: every enabled, non-deleted product, with only
// its id, name and description (no token counts, no creator: every signed-in user sees
// this list).
func (h *Handler) ListMintableProducts(w http.ResponseWriter, r *http.Request) {
	rows, err := h.q.ListEnabledProducts(r.Context())
	if err != nil {
		slog.Error("list mintable products", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]apitypes.MintableProductDTO, 0, len(rows))
	for _, p := range rows {
		out = append(out, apitypes.MintableProductDTO{ID: p.ID.String(), Name: p.Name, Description: p.Description})
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"products": out})
}

// MintProductToken mints a product token for the caller and returns its plaintext value
// exactly once (only the sha256 is stored). See the file comment for the status codes.
//
// ONE transaction, in this order:
//  1. LockProductTokenMint(user, product): a transaction-scoped advisory lock, so two
//     concurrent mints for the same pair run one after the other;
//  2. re-read the product INSIDE the transaction and refuse unless it is enabled and not
//     deleted (unknown: 404; disabled or deleted: 409);
//  3. CountActiveProductTokensForUserProduct >= maxActiveProductTokensPerProduct: 409;
//  4. generate the token and CreateProductToken, whose INSERT is itself guarded on the
//     product being enabled and live (no row: 409);
//  5. commit.
//
// TestMintProductTokenCapRaceLiveDB races two mints for one pair that already holds
// cap-1 active tokens and pins that exactly one succeeds and the cap's worth end up
// active; without the lock both pass the count and an eleventh row lands.
func (h *Handler) MintProductToken(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var req mintProductTokenRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	v, err := validateMintProductToken(req)
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return
	}

	var (
		token   string
		row     store.CreateProductTokenRow
		product store.Product
	)
	err = h.inTx(r.Context(), func(q *store.Queries) error {
		if err := q.LockProductTokenMint(r.Context(), store.LockProductTokenMintParams{UserID: user.ID, ProductID: v.productID}); err != nil {
			return err
		}
		var err error
		if product, err = q.GetProduct(r.Context(), v.productID); err != nil {
			return err // ErrNoRows: unknown product
		}
		if !product.Enabled || product.DeletedAt.Valid {
			return errMintProductUnavailable
		}
		n, err := q.CountActiveProductTokensForUserProduct(r.Context(), store.CountActiveProductTokensForUserProductParams{
			UserID: user.ID, ProductID: v.productID,
		})
		if err != nil {
			return err
		}
		if n >= maxActiveProductTokensPerProduct {
			return errMintProductTokenCap
		}
		var (
			hash   []byte
			prefix string
		)
		if token, hash, prefix, err = producttoken.Generate(); err != nil {
			return err
		}
		row, err = q.CreateProductToken(r.Context(), store.CreateProductTokenParams{
			UserID:      user.ID,
			ProductID:   v.productID,
			Name:        v.name,
			TokenHash:   hash,
			TokenPrefix: prefix,
			Scopes:      v.scopes,
			ExpiresAt:   v.expiresAt(time.Now()),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			// The guarded INSERT found the product not mintable after all.
			return errMintProductUnavailable
		}
		return err
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		httpx.Error(w, http.StatusNotFound, "product not found")
		return
	case errors.Is(err, errMintProductUnavailable):
		httpx.Error(w, http.StatusConflict, "product is disabled or deleted; no token can be minted for it")
		return
	case errors.Is(err, errMintProductTokenCap):
		httpx.Error(w, http.StatusConflict, fmt.Sprintf(
			"you already have %d active tokens for this product; revoke one before minting another",
			maxActiveProductTokensPerProduct))
		return
	case err != nil:
		slog.Error("mint product token", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}

	var ip fmt.Stringer
	if row.LastUsedIp != nil {
		ip = row.LastUsedIp
	}
	slog.Info("product token minted", "user_id", user.ID, "product_id", row.ProductID,
		"token_id", row.ID, "token_prefix", row.TokenPrefix)
	httpx.JSON(w, http.StatusCreated, apitypes.MintProductTokenResponse{
		Token: token,
		ProductToken: productTokenDTO(row.ID, row.ProductID, product.Name, row.Name, row.TokenPrefix,
			row.Scopes, row.Revoked, row.CreatedAt, row.LastUsedAt, row.ExpiresAt, ip),
	})
}

// RevokeMyProductToken revokes one of the caller's product tokens. Owner-scoped: a
// foreign, unknown or already-revoked id is a 404, never a cross-user revoke.
//
// A MANUAL token (grant_id NULL) is revoked alone. An OAuth access token (grant_id set) belongs to
// a connection: grant tokens are no longer listed on this route's page, but the id stays reachable,
// and revoking one revokes its whole grant (PRD #1910 D6) in one transaction, taking the grant lock
// first (D8), because revoking just that token would let the product refresh a new one.
func (h *Handler) RevokeMyProductToken(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	id, ok := httpx.PathUUID(w, r, "id", "token")
	if !ok {
		return
	}
	grantID, err := h.q.GetOwnProductTokenGrantID(r.Context(), store.GetOwnProductTokenGrantIDParams{ID: id, UserID: user.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.Error(w, http.StatusNotFound, "token not found")
		return
	}
	if err != nil {
		slog.Error("revoke product token", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if grantID.Valid {
		owner := user.ID
		err := h.inTx(r.Context(), func(q *store.Queries) error {
			return revokeGrantOfTokenTx(r.Context(), q, grantID.Bytes, &owner)
		})
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			httpx.Error(w, http.StatusNotFound, "token not found")
		case err != nil:
			slog.Error("revoke product token's grant", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
		default:
			w.WriteHeader(http.StatusNoContent)
		}
		return
	}
	n, err := h.q.RevokeProductToken(r.Context(), store.RevokeProductTokenParams{ID: id, UserID: user.ID})
	if err != nil {
		slog.Error("revoke product token", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if n == 0 {
		httpx.Error(w, http.StatusNotFound, "token not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
