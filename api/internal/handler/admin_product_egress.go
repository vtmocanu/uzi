package handler

// admin_product_egress.go is the admin API for a product's site-list allowance (PRD #1976 M1): the
// egress profiles (site lists) a product's tokens may name when they create a job. Routing
// (routes_admin.go) carries the authorization: GET sits in the admin READ group (session or uza_
// token), PUT and DELETE in the cookie-only WRITE group (RequireAuth + RequireAdmin), so no Bearer
// token ever grants or revokes an allowance. A user (uzc_) token is not limited by this table.
//
// An allowance only gates NEW job creates: removing one never changes a job already created.

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// productEgressProfileDTO is one allowed site list: the profile's name and description and when it
// was allowed for the product. The list envelope is {"egress_profiles": [...]}, ordered by name.
type productEgressProfileDTO struct {
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// productEgressList reads a product's allowed lists. A false ok means the response is written.
func (h *Handler) productEgressList(w http.ResponseWriter, r *http.Request, q *store.Queries, id uuid.UUID) (map[string]any, bool) {
	rows, err := q.ListProductEgressProfiles(r.Context(), id)
	if err != nil {
		slog.Error("admin list product egress profiles", "product_id", id, "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return nil, false
	}
	out := make([]productEgressProfileDTO, 0, len(rows))
	for _, p := range rows {
		out = append(out, productEgressProfileDTO{Name: p.Name, Description: p.Description, CreatedAt: p.CreatedAt.Time})
	}
	return map[string]any{"egress_profiles": out}, true
}

// AdminListProductEgressProfiles serves GET /api/admin/products/{id}/egress-profiles. 404 for an
// unknown product; a soft-deleted product still lists (the audit trail), like AdminGetProductSkills.
func (h *Handler) AdminListProductEgressProfiles(w http.ResponseWriter, r *http.Request) {
	id, ok := httpx.PathUUID(w, r, "id", "product")
	if !ok {
		return
	}
	if _, err := h.q.GetProduct(r.Context(), id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Error(w, http.StatusNotFound, "product not found")
			return
		}
		slog.Error("admin list product egress profiles: get product", "product_id", id, "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	body, ok := h.productEgressList(w, r, h.q, id)
	if !ok {
		return
	}
	httpx.JSON(w, http.StatusOK, body)
}

// productEgressTarget resolves the product (must exist and be live) and the profile named in the
// path for a write. It writes the refusal and reports false otherwise: 404 unknown product or
// profile, 409 deleted product.
func (h *Handler) productEgressTarget(w http.ResponseWriter, r *http.Request) (productID uuid.UUID, profileID uuid.UUID, ok bool) {
	id, ok := httpx.PathUUID(w, r, "id", "product")
	if !ok {
		return productID, profileID, false
	}
	name, ok := egressProfileName(w, r)
	if !ok {
		return productID, profileID, false
	}
	p, err := h.q.GetProduct(r.Context(), id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Error(w, http.StatusNotFound, "product not found")
			return productID, profileID, false
		}
		slog.Error("admin product egress profile: get product", "product_id", id, "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return productID, profileID, false
	}
	if p.DeletedAt.Valid {
		httpx.Error(w, http.StatusConflict, "product is deleted")
		return productID, profileID, false
	}
	prof, err := h.q.GetEgressProfileByName(r.Context(), name)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Error(w, http.StatusNotFound, "egress profile not found")
			return productID, profileID, false
		}
		slog.Error("admin product egress profile: get profile", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return productID, profileID, false
	}
	return id, prof.ID, true
}

// AdminAllowProductEgressProfile serves PUT /api/admin/products/{id}/egress-profiles/{name}: allow
// the product to use the list. Idempotent (an existing allowance is kept as is); no body. 200 with
// the product's allowed lists after the write.
func (h *Handler) AdminAllowProductEgressProfile(w http.ResponseWriter, r *http.Request) {
	actor, ok := egressProfileAdmin(w, r)
	if !ok {
		return
	}
	productID, profileID, ok := h.productEgressTarget(w, r)
	if !ok {
		return
	}
	if err := h.q.AddProductEgressProfile(r.Context(), store.AddProductEgressProfileParams{
		ProductID: productID, EgressProfileID: profileID, CreatedBy: pgconv.UUID(actor.ID),
	}); err != nil {
		slog.Error("admin allow product egress profile", "product_id", productID, "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	body, ok := h.productEgressList(w, r, h.q, productID)
	if !ok {
		return
	}
	httpx.JSON(w, http.StatusOK, body)
}

// AdminRevokeProductEgressProfile serves DELETE /api/admin/products/{id}/egress-profiles/{name}:
// remove the allowance. 204; 404 when the product, the list or the allowance does not exist.
func (h *Handler) AdminRevokeProductEgressProfile(w http.ResponseWriter, r *http.Request) {
	if _, ok := egressProfileAdmin(w, r); !ok {
		return
	}
	productID, profileID, ok := h.productEgressTarget(w, r)
	if !ok {
		return
	}
	n, err := h.q.RemoveProductEgressProfile(r.Context(), store.RemoveProductEgressProfileParams{
		ProductID: productID, EgressProfileID: profileID,
	})
	if err != nil {
		slog.Error("admin revoke product egress profile", "product_id", productID, "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if n == 0 {
		httpx.Error(w, http.StatusNotFound, "this product is not allowed to use that egress profile")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
