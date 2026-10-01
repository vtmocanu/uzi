package handler

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/oauthsrv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Admin writes that make a product an OAuth client (PRD #1910 M1, D2). Both sit in the
// cookie-only WRITE group of routes_admin.go (RequireAuth + RequireAdmin, with the session's
// CSRF check), so a Bearer of any class is refused before a handler here runs.

// setProductOAuthRequest is the PUT /api/admin/products/{id}/oauth body. Both keys are
// required (pointers distinguish absent from empty), so a client that forgets one cannot
// silently clear the registration.
type setProductOAuthRequest struct {
	RedirectURIs *[]string `json:"redirect_uris"`
	Scopes       *[]string `json:"scopes"`
}

// AdminSetProductOAuth replaces a live product's OAuth redirect URIs and allowed scopes:
// {redirect_uris: [...], scopes: [...]}. Both lists are validated with oauthsrv (D2): at most
// 5 redirect URIs, each https or a loopback-IP http URL, no fragment or userinfo, no
// duplicates; scopes a non-empty, duplicate-free subset of the product-token scopes.
//
// The two lists are set together or cleared together: non-empty redirect_uris require a
// non-empty scopes list, and empty redirect_uris require empty scopes (a 400 otherwise).
// Sending both empty is how an admin stops a product being a client; the client secret is
// untouched (rotate it separately), so re-registering later keeps the existing secret.
//
// An unknown id is a 404; a soft-deleted product is a 409, like AdminPatchProduct (it still
// exists and is still listed, and a deleted product can never be changed). The response is
// {product: ProductDTO}; the secret and its hash are never in it.
func (h *Handler) AdminSetProductOAuth(w http.ResponseWriter, r *http.Request) {
	actor, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	id, ok := httpx.PathUUID(w, r, "id", "product")
	if !ok {
		return
	}
	var req setProductOAuthRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.RedirectURIs == nil || req.Scopes == nil {
		httpx.Error(w, http.StatusBadRequest, "redirect_uris and scopes are both required (send empty lists to clear)")
		return
	}
	uris := *req.RedirectURIs
	scopes := []string{}
	if err := oauthsrv.ValidateRedirectURIs(uris); err != nil {
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	switch {
	case len(uris) > 0:
		validated, err := oauthsrv.ValidateScopes(*req.Scopes)
		if err != nil {
			httpx.Error(w, http.StatusBadRequest, err.Error())
			return
		}
		scopes = validated
	case len(*req.Scopes) > 0:
		httpx.Error(w, http.StatusBadRequest, "scopes must be empty when redirect_uris is empty: clear both to stop being an OAuth client")
		return
	}
	if uris == nil {
		uris = []string{}
	}

	var (
		updated store.Product
		active  int64
		conns   int64
	)
	err := h.inTx(r.Context(), func(q *store.Queries) error {
		var err error
		updated, err = q.SetProductOAuthClient(r.Context(), store.SetProductOAuthClientParams{
			ID:           id,
			RedirectUris: uris,
			OauthScopes:  scopes,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			// No live row: unknown (ErrNoRows from GetProduct, a 404) or soft-deleted.
			if _, err := q.GetProduct(r.Context(), id); err != nil {
				return err
			}
			return errProductDeleted
		}
		if err != nil {
			return err
		}
		active, conns, err = productUsageCounts(r.Context(), q, id)
		return err
	})
	if writeProductOAuthError(w, err, "admin set product oauth") {
		return
	}
	slog.Info("admin set product oauth client", "actor_id", actor.ID, "product_id", id,
		"redirect_uris", len(uris), "scopes", len(scopes))
	httpx.JSON(w, http.StatusOK, map[string]any{"product": productDTO(updated, active, conns)})
}

// AdminRotateProductClientSecret mints a new client secret (uzs_, 256 bits) for a live
// product, replacing any previous one immediately. Only the sha256 and a short display prefix
// are stored; the plaintext is in this response and nowhere else, so losing it means rotating
// again. The response is apitypes.RotateProductClientSecretResponse, marked no-store.
//
// A product with no redirect URIs or scopes yet can hold a secret (it is simply not a client
// until they are set, is_client false). Unknown id 404, soft-deleted 409, as AdminSetProductOAuth.
func (h *Handler) AdminRotateProductClientSecret(w http.ResponseWriter, r *http.Request) {
	actor, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	id, ok := httpx.PathUUID(w, r, "id", "product")
	if !ok {
		return
	}
	secret, hash, prefix, err := oauthsrv.GenerateSecret()
	if err != nil {
		slog.Error("admin rotate product client secret: generate", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	var (
		updated store.Product
		active  int64
		conns   int64
	)
	err = h.inTx(r.Context(), func(q *store.Queries) error {
		var err error
		updated, err = q.RotateProductClientSecret(r.Context(), store.RotateProductClientSecretParams{
			ID:                 id,
			ClientSecretHash:   hash,
			ClientSecretPrefix: prefix,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			if _, err := q.GetProduct(r.Context(), id); err != nil {
				return err
			}
			return errProductDeleted
		}
		if err != nil {
			return err
		}
		active, conns, err = productUsageCounts(r.Context(), q, id)
		return err
	})
	if writeProductOAuthError(w, err, "admin rotate product client secret") {
		return
	}
	// A credential-affecting admin action with no per-request audit log: leave a breadcrumb,
	// never the secret or its prefix.
	slog.Info("admin rotated product client secret", "actor_id", actor.ID, "product_id", id)
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, http.StatusOK, apitypes.RotateProductClientSecretResponse{
		ClientSecret: secret,
		Product:      productDTO(updated, active, conns),
	})
}

// writeProductOAuthError maps the transaction error of the two OAuth writes to a response and
// reports whether it wrote one (true) or the caller should respond with success (false).
func writeProductOAuthError(w http.ResponseWriter, err error, logMsg string) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, pgx.ErrNoRows):
		httpx.Error(w, http.StatusNotFound, "product not found")
	case errors.Is(err, errProductDeleted):
		httpx.Error(w, http.StatusConflict, "product is deleted; a deleted product cannot be changed")
	default:
		slog.Error(logMsg, "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
	}
	return true
}
