package handler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/termsafe"
)

// The admin product registry and product-credential inventory (PRD #1907 M4).
//
// Routing (routes_admin.go) carries the authorization, as for every admin route:
//
//   - GET /api/admin/products and GET /api/admin/product-tokens sit in the READ group
//     (RequireUser + RequireAdminRO), so an admin session or a uza_ token can read them.
//   - Every write sits in the cookie-only WRITE group (RequireAuth + RequireAdmin, with
//     the session's CSRF check), so a Bearer of any class is refused before a handler
//     here runs (D14).
//
// None of these handlers re-checks admin-ness or token scope: one mechanism, one place.

// maxProductNameBytes and maxProductDescriptionBytes are the D11 caps. The name cap is
// the CLI-token label cap on purpose: both are one-line labels printed in a listing
// another person reads.
const (
	maxProductNameBytes        = maxCLITokenNameBytes
	maxProductDescriptionBytes = 1000
)

// validateProductName trims raw and returns it when it is a displayable product name:
// non-empty, at most maxProductNameBytes bytes, and round-trip-clean under
// termsafe.Validate (no control characters, no invisible formatting characters such
// as bidi overrides or zero-width joiners, valid UTF-8). The name is shown to every
// user in the mint picker and beside owner emails in the admin inventory (D11).
func validateProductName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" || len(name) > maxProductNameBytes {
		return "", fmt.Errorf("name must be non-empty and at most %d bytes", maxProductNameBytes)
	}
	if err := termsafe.Validate("name", name); err != nil {
		return "", err
	}
	return name, nil
}

// validateProductDescription trims raw and returns it when it is a displayable
// description: at most maxProductDescriptionBytes bytes and termsafe-clean. Empty is
// allowed. A description is ONE line: termsafe.Validate rejects every control
// character, newline and tab included, because the description is printed in listings
// (the mint picker, `uzi admin products`) where an embedded newline can forge a row.
func validateProductDescription(raw string) (string, error) {
	desc := strings.TrimSpace(raw)
	if len(desc) > maxProductDescriptionBytes {
		return "", fmt.Errorf("description must be at most %d bytes", maxProductDescriptionBytes)
	}
	if err := termsafe.Validate("description", desc); err != nil {
		return "", err
	}
	return desc, nil
}

// validateAllowedJobTypes checks each entry against the known job types (runkind.JobTypes,
// the mirror of the products_allowed_job_types_check CHECK) and returns the list de-duplicated
// in first-seen order, never nil. An unknown entry is an error naming it; the empty list is
// valid (the product may create no job).
func validateAllowedJobTypes(in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	for _, t := range in {
		if !slices.Contains(runkind.JobTypes(), t) {
			return nil, fmt.Errorf("allowed_job_types: unknown job type %q (known: %s)", t, strings.Join(runkind.JobTypes(), ", "))
		}
		if !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	return out, nil
}

// jobTypesOrEmpty is the wire form of a products text[] column (allowed_job_types, redirect_uris, oauth_scopes): never nil.
func jobTypesOrEmpty(in []string) []string {
	if in == nil {
		return []string{}
	}
	return slices.Clone(in)
}

func productDTO(p store.Product, activeTokens, liveConnections int64) apitypes.ProductDTO {
	return apitypes.ProductDTO{
		AllowedJobTypes:     jobTypesOrEmpty(p.AllowedJobTypes),
		ID:                  p.ID.String(),
		Name:                p.Name,
		Description:         p.Description,
		Enabled:             p.Enabled,
		DeletedAt:           timePtr(p.DeletedAt.Valid, p.DeletedAt.Time),
		CreatedAt:           p.CreatedAt.Time,
		ActiveTokenCount:    activeTokens,
		LiveConnectionCount: liveConnections,
		OAuthClient: oauthClientDTO(p.RedirectUris, p.OauthScopes, p.ClientSecretHash != nil,
			p.ClientSecretPrefix, p.ClientSecretRotatedAt),
	}
}

// oauthClientDTO builds the nested oauth_client view (PRD #1910 D2) from a product's client
// columns. It takes the secret as a bool plus the display prefix and rotation time, never the
// hash itself, so the hash cannot reach a DTO through this path. IsClient is derived here, in
// the one place both the single-product and list builders share.
func oauthClientDTO(uris, scopes []string, hasSecret bool, prefix pgtype.Text, rotatedAt pgtype.Timestamptz) apitypes.ProductOAuthClientDTO {
	dto := apitypes.ProductOAuthClientDTO{
		RedirectURIs: jobTypesOrEmpty(uris),
		Scopes:       jobTypesOrEmpty(scopes),
		HasSecret:    hasSecret,
		RotatedAt:    timePtr(rotatedAt.Valid, rotatedAt.Time),
	}
	if prefix.Valid {
		dto.SecretPrefix = prefix.String
	}
	dto.IsClient = len(dto.RedirectURIs) > 0 && len(dto.Scopes) > 0 && hasSecret
	return dto
}

// AdminListProducts returns every registered product, soft-deleted ones included (they
// stay listed for the audit trail, D9), each with its count of active tokens.
func (h *Handler) AdminListProducts(w http.ResponseWriter, r *http.Request) {
	rows, err := h.q.ListProducts(r.Context())
	if err != nil {
		slog.Error("admin list products", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]apitypes.ProductDTO, 0, len(rows))
	for _, p := range rows {
		out = append(out, apitypes.ProductDTO{
			ID:                  p.ID.String(),
			Name:                p.Name,
			Description:         p.Description,
			Enabled:             p.Enabled,
			DeletedAt:           timePtr(p.DeletedAt.Valid, p.DeletedAt.Time),
			CreatedAt:           p.CreatedAt.Time,
			ActiveTokenCount:    p.ActiveTokenCount,
			LiveConnectionCount: p.LiveConnectionCount,
			AllowedJobTypes:     jobTypesOrEmpty(p.AllowedJobTypes),
			OAuthClient:         oauthClientDTO(p.RedirectUris, p.OauthScopes, p.HasClientSecret, p.ClientSecretPrefix, p.ClientSecretRotatedAt),
		})
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"products": out})
}

// maxAdminProductTokenRows bounds one GET /api/admin/product-tokens response (PRD #1907
// M4 security audit, H1). Revoked rows are kept for the audit trail, so without a bound
// a user looping mint -> revoke would grow every admin load forever.
const maxAdminProductTokenRows = 1000

// AdminListProductTokens is the product-credential inventory, the sibling of
// AdminListCLITokens: product tokens with their owner and product, metadata only. The
// store query projects its columns without token_hash, so the hash is not even in the
// row type this reads. Revoked tokens and tokens of soft-deleted products are included;
// they are the incident trail (D9).
//
// BOUNDED: at most maxAdminProductTokenRows (1000) rows per load, ACTIVE tokens (not
// revoked, not expired) first and then newest first. The cut drops revoked/expired
// history before any active token, but past 1000 ACTIVE tokens the oldest active ones
// are cut as well. The query is asked for one row more than the bound, so the response
// says whether anything was cut: {"tokens": [...], "truncated": bool}. The per-user mint
// limiter (authLimiter on POST /api/me/product-tokens) bounds how fast any one user can
// add rows.
func (h *Handler) AdminListProductTokens(w http.ResponseWriter, r *http.Request) {
	bound := productTokenListBound(h.adminProductTokenRowsOverride, maxAdminProductTokenRows)
	rows, err := h.q.ListAllProductTokensForAdmin(r.Context(), bound+1)
	if err != nil {
		slog.Error("admin list product tokens", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	rows, truncated := cutProductTokenList(rows, bound)
	out := make([]apitypes.AdminProductTokenDTO, 0, len(rows))
	for _, t := range rows {
		dto := apitypes.AdminProductTokenDTO{
			ProductTokenDTO: apitypes.ProductTokenDTO{
				ID:          t.ID.String(),
				ProductID:   t.ProductID.String(),
				ProductName: t.ProductName,
				Name:        t.Name,
				TokenPrefix: t.TokenPrefix,
				Scopes:      t.Scopes,
				Revoked:     t.Revoked,
				CreatedAt:   t.CreatedAt.Time,
				LastUsedAt:  timePtr(t.LastUsedAt.Valid, t.LastUsedAt.Time),
				ExpiresAt:   timePtr(t.ExpiresAt.Valid, t.ExpiresAt.Time),
			},
			UserID:     t.UserID.String(),
			OwnerEmail: t.OwnerEmail,
		}
		// The column is NOT NULL and non-empty, but the DTO promises a non-null array.
		if dto.Scopes == nil {
			dto.Scopes = []string{}
		}
		if t.LastUsedIp != nil {
			s := t.LastUsedIp.String()
			dto.LastUsedIP = &s
		}
		out = append(out, dto)
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"tokens": out, "truncated": truncated})
}

// AdminCreateProduct registers a product: {name, description}. The creating admin is
// recorded from the session, never the body. A live product with the same name,
// compared case-insensitively (uq_products_live_name), is a 409; a soft-deleted
// product's name is free for reuse.
func (h *Handler) AdminCreateProduct(w http.ResponseWriter, r *http.Request) {
	actor, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var req struct {
		Name            string   `json:"name"`
		Description     string   `json:"description"`
		AllowedJobTypes []string `json:"allowed_job_types"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	allowed, err := validateAllowedJobTypes(req.AllowedJobTypes)
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	name, err := validateProductName(req.Name)
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	desc, err := validateProductDescription(req.Description)
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	p, err := h.q.CreateProduct(r.Context(), store.CreateProductParams{
		Name:        name,
		Description: desc,
		CreatedBy:   pgtype.UUID{Bytes: actor.ID, Valid: true},
		// Never nil: an omitted list is the empty allow-list (fail-closed), explicitly.
		AllowedJobTypes: allowed,
	})
	if err != nil {
		if isUniqueViolation(err) {
			httpx.Error(w, http.StatusConflict, "a product with this name already exists")
			return
		}
		slog.Error("admin create product", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusCreated, map[string]any{"product": productDTO(p, 0, 0)})
}

// errProductDeleted is the PATCH/DELETE refusal for a soft-deleted product.
var errProductDeleted = errors.New("product is deleted")

// AdminPatchProduct edits a live product's description, enabled flag and/or allowed job
// types. Fields the body omits keep their value (an explicit null counts as omitted; an
// empty allowed_job_types list clears the allow-list); a body naming none is a 400. The name is immutable
// (it is the label users recognise their tokens by).
//
// Disabling a product makes every one of its tokens fail the /api/v1 auth lookup on
// the next request (D8); re-enabling a disabled, live product restores them.
//
// A SOFT-DELETED PRODUCT IS A 409, NOT A 404: it still exists and is still listed by
// GET /api/admin/products, so "not found" would contradict the list. Deletion is final
// (D9): a deleted product can never be re-enabled or edited, and UpdateProduct's
// deleted_at guard plus the products_deleted_is_disabled CHECK back this handler up.
// An unknown id is a 404.
//
// The skills source (PRD #1909 D9) is edited here too: skills_repo_url (https, no userinfo, on
// UZI_PRODUCT_SKILLS_ALLOWED_BASE_URLS; "" clears it), skills_ref, and the WRITE-ONLY clone token
// (skills_token seals a new one, clear_skills_token removes it; the response only ever says
// skills_token_set through GET .../skills). Changing the repo URL or ref discards a staged
// snapshot of the old source.
func (h *Handler) AdminPatchProduct(w http.ResponseWriter, r *http.Request) {
	actor, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	id, ok := httpx.PathUUID(w, r, "id", "product")
	if !ok {
		return
	}
	var req struct {
		Description     *string   `json:"description"`
		Enabled         *bool     `json:"enabled"`
		AllowedJobTypes *[]string `json:"allowed_job_types"`
		// The skills source (PRD #1909 M6). SkillsToken is write-only.
		SkillsRepoURL    *string `json:"skills_repo_url"`
		SkillsRef        *string `json:"skills_ref"`
		SkillsToken      *string `json:"skills_token"`
		ClearSkillsToken *bool   `json:"clear_skills_token"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	skillsPatch, serr := h.parseSkillsSourcePatch(id, req.SkillsRepoURL, req.SkillsRef, req.SkillsToken, req.ClearSkillsToken)
	if serr != nil {
		httpx.Error(w, http.StatusBadRequest, serr.Error())
		return
	}
	if req.Description == nil && req.Enabled == nil && req.AllowedJobTypes == nil && !skillsPatch.set() {
		httpx.Error(w, http.StatusBadRequest, "nothing to update: set description, enabled, allowed_job_types and/or the skills source")
		return
	}
	var allowed []string
	if req.AllowedJobTypes != nil {
		a, err := validateAllowedJobTypes(*req.AllowedJobTypes)
		if err != nil {
			httpx.Error(w, http.StatusBadRequest, err.Error())
			return
		}
		allowed = a
	}
	var desc *string
	if req.Description != nil {
		d, err := validateProductDescription(*req.Description)
		if err != nil {
			httpx.Error(w, http.StatusBadRequest, err.Error())
			return
		}
		desc = &d
	}

	// UpdateProduct takes each field as a nullable argument and COALESCEs NULL to the
	// column's current value in the same statement, so no read-merge happens here and two
	// concurrent PATCHes of different fields cannot overwrite each other.
	params := store.UpdateProductParams{ID: id}
	if desc != nil {
		params.Description = pgtype.Text{String: *desc, Valid: true}
	}
	if req.Enabled != nil {
		params.Enabled = pgtype.Bool{Bool: *req.Enabled, Valid: true}
	}
	// allowed is non-nil (possibly empty) exactly when the body named the field; nil is NULL,
	// which UpdateProduct COALESCEs to the current value.
	params.AllowedJobTypes = allowed
	var (
		updated store.Product
		active  int64
		conns   int64
	)
	err := h.inTx(r.Context(), func(q *store.Queries) error {
		var err error
		updated, err = q.UpdateProduct(r.Context(), params)
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
		if skillsPatch.set() {
			if updated, err = applySkillsSourcePatch(r.Context(), q, updated, skillsPatch); err != nil {
				return err
			}
		}
		active, conns, err = productUsageCounts(r.Context(), q, id)
		return err
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		httpx.Error(w, http.StatusNotFound, "product not found")
		return
	case errors.Is(err, errProductDeleted):
		httpx.Error(w, http.StatusConflict, "product is deleted; a deleted product cannot be changed or re-enabled")
		return
	case err != nil:
		slog.Error("admin patch product", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if req.Enabled != nil {
		// A credential-affecting admin action (it turns every token of the product off
		// or back on): leave the same kind of breadcrumb CreateCLIToken leaves for an
		// admin_ro mint, since there is no per-request audit log.
		slog.Info("admin set product enabled", "actor_id", actor.ID, "product_id", id,
			"enabled", updated.Enabled, "active_tokens", active)
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"product": productDTO(updated, active, conns)})
}

// productUsageCounts reads the two figures a ProductDTO carries beside the row: the product's
// active MANUAL tokens and its live OAuth connections (PRD #1910 D5). Run on the caller's
// transaction so both are read at the same point as the write they accompany.
func productUsageCounts(ctx context.Context, q *store.Queries, id uuid.UUID) (tokens, conns int64, err error) {
	if tokens, err = q.CountActiveProductTokensForProduct(ctx, id); err != nil {
		return 0, 0, err
	}
	if conns, err = q.CountLiveOAuthGrantsForProduct(ctx, id); err != nil {
		return 0, 0, err
	}
	return tokens, conns, nil
}

// AdminDeleteProduct soft-deletes a product (D9): deleted_at is set and the product is
// disabled in one statement, so every one of its tokens is refused on its next
// request, while the token rows stay for the audit trail.
//
// The response is apitypes.AdminDeleteProductResponse, {product, stopped_token_count,
// stopped_connection_count}: stopped_token_count is the number of MANUAL tokens (not the
// access tokens of OAuth connections, PRD #1910 D5) this delete made unusable and
// stopped_connection_count the number of live OAuth connections it made unusable. For a
// product that was enabled at deletion those are its manual tokens neither revoked nor
// expired and its grants not revoked; for a product that was ALREADY DISABLED both are 0,
// because the disable had already refused them. Nothing is revoked: the grants and tokens
// stay as they are, only refused. The pre-delete enabled flag is read under
// GetProductForUpdate's row lock in the deleting transaction. product.active_token_count and
// product.live_connection_count keep their registry meaning (whatever the state), which are
// the figures the UI's delete confirm reads ahead of time from GET /api/admin/products.
//
// An unknown id is a 404; an already-deleted product is a 409 (it exists and is
// listed, see AdminPatchProduct).
func (h *Handler) AdminDeleteProduct(w http.ResponseWriter, r *http.Request) {
	actor, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	id, ok := httpx.PathUUID(w, r, "id", "product")
	if !ok {
		return
	}
	var (
		deleted      store.Product
		active       int64
		conns        int64
		stopped      int64
		stoppedConns int64
	)
	err := h.inTx(r.Context(), func(q *store.Queries) error {
		cur, err := q.GetProductForUpdate(r.Context(), id)
		if err != nil {
			return err // ErrNoRows: unknown id
		}
		if cur.DeletedAt.Valid {
			return errProductDeleted
		}
		n, err := q.SoftDeleteProduct(r.Context(), id)
		if err != nil {
			return err
		}
		if n == 0 {
			return errProductDeleted // unreachable under the row lock; fail closed
		}
		if deleted, err = q.GetProduct(r.Context(), id); err != nil {
			return err
		}
		if active, conns, err = productUsageCounts(r.Context(), q, id); err != nil {
			return err
		}
		if cur.Enabled {
			stopped, stoppedConns = active, conns
		}
		return nil
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		httpx.Error(w, http.StatusNotFound, "product not found")
		return
	case errors.Is(err, errProductDeleted):
		httpx.Error(w, http.StatusConflict, "product is already deleted")
		return
	case err != nil:
		slog.Error("admin delete product", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	slog.Info("admin deleted product", "actor_id", actor.ID, "product_id", id,
		"stopped_tokens", stopped, "stopped_connections", stoppedConns)
	httpx.JSON(w, http.StatusOK, apitypes.AdminDeleteProductResponse{
		Product:                productDTO(deleted, active, conns),
		StoppedTokenCount:      stopped,
		StoppedConnectionCount: stoppedConns,
	})
}

// AdminRevokeProductToken revokes one product token by id (D8). Product tokens are
// product credentials, so an admin may revoke one; PRD #64's rule that an admin never
// revokes a user's CLI token is unchanged (this touches product_tokens only). An
// unknown or already-revoked id is a 404, like the owner's own revoke.
//
// A token that belongs to an OAuth grant (grant_id set) revokes its whole grant (PRD #1910 D6),
// in one transaction with the grant lock first (D8); otherwise the product would just refresh.
func (h *Handler) AdminRevokeProductToken(w http.ResponseWriter, r *http.Request) {
	actor, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	id, ok := httpx.PathUUID(w, r, "id", "token")
	if !ok {
		return
	}
	grantID, err := h.q.GetProductTokenGrantID(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.Error(w, http.StatusNotFound, "token not found")
		return
	}
	if err != nil {
		slog.Error("admin revoke product token", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if grantID.Valid {
		err := h.inTx(r.Context(), func(q *store.Queries) error {
			return revokeGrantOfTokenTx(r.Context(), q, grantID.Bytes, nil)
		})
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			httpx.Error(w, http.StatusNotFound, "token not found")
		case err != nil:
			slog.Error("admin revoke product token's grant", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
		default:
			slog.Info("admin revoked oauth grant of product token", "actor_id", actor.ID, "token_id", id, "grant_id", uuid.UUID(grantID.Bytes).String())
			w.WriteHeader(http.StatusNoContent)
		}
		return
	}
	n, err := h.q.AdminRevokeProductToken(r.Context(), id)
	if err != nil {
		slog.Error("admin revoke product token", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if n == 0 {
		httpx.Error(w, http.StatusNotFound, "token not found")
		return
	}
	slog.Info("admin revoked product token", "actor_id", actor.ID, "token_id", id)
	w.WriteHeader(http.StatusNoContent)
}

// inTx runs fn on a transaction-bound Queries and commits when fn returns nil; any
// error from fn (or the commit) rolls back and is returned unchanged, so callers can
// errors.Is it.
func (h *Handler) inTx(ctx context.Context, fn func(q *store.Queries) error) error {
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after a successful Commit
	if err := fn(h.q.WithTx(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
