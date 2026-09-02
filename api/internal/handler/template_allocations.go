package handler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// templateAllocationDTO is one template in the caller's allocation view: its
// identity/scope plus whether it is a global default, the caller's own overlay
// (null = none), and the resolved effective decision (overlay wins, else the
// global default). This is what the Agents page renders as per-template toggles.
type templateAllocationDTO struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Description   string `json:"description"`
	Scope         string `json:"scope"`
	IsBuiltin     bool   `json:"is_builtin"`
	GlobalDefault bool   `json:"global_default"`
	MyOverride    *bool  `json:"my_override"`
	Effective     bool   `json:"effective"`
}

// templateOverrideInput is one of the caller's overlay decisions.
type templateOverrideInput struct {
	TemplateID string `json:"template_id"`
	Enabled    bool   `json:"enabled"`
}

// templateAllocationsWriteRequest is the replace-set write. A nil pointer means
// "leave this half untouched"; a non-nil (even empty) slice fully replaces that
// half. global_default_ids is admin-only (the shared default set); my_overrides
// is any user's own overlay.
type templateAllocationsWriteRequest struct {
	GlobalDefaultIDs *[]string                `json:"global_default_ids"`
	MyOverrides      *[]templateOverrideInput `json:"my_overrides"`
}

// GetTemplateAllocations returns the caller's per-template allocation view over
// every template visible to them (builtin ∪ global ∪ own).
func (h *Handler) GetTemplateAllocations(w http.ResponseWriter, r *http.Request) {
	actor, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	dto, ok := h.loadTemplateAllocations(w, r.Context(), actor)
	if !ok {
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"templates": dto})
}

// SetTemplateAllocations replaces the global-default set (admin) and/or the
// caller's overlay. global_default_ids may reference only builtin/global
// templates; my_overrides may reference any template visible to the caller. Each
// provided half is fully replaced.
func (h *Handler) SetTemplateAllocations(w http.ResponseWriter, r *http.Request) {
	actor, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var req templateAllocationsWriteRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.GlobalDefaultIDs == nil && req.MyOverrides == nil {
		httpx.Error(w, http.StatusBadRequest, "provide global_default_ids and/or my_overrides")
		return
	}

	ctx := r.Context()
	var globalIDs []uuid.UUID
	if req.GlobalDefaultIDs != nil {
		if !actor.IsAdmin {
			httpx.Error(w, http.StatusForbidden, "only admins can set global default allocations")
			return
		}
		ids, err := h.resolveGlobalDefaultable(ctx, actor, *req.GlobalDefaultIDs)
		if err != nil {
			httpx.Error(w, http.StatusBadRequest, err.Error())
			return
		}
		globalIDs = ids
	}
	var overrides []resolvedOverride
	if req.MyOverrides != nil {
		ov, err := h.resolveOverrides(ctx, actor, *req.MyOverrides)
		if err != nil {
			httpx.Error(w, http.StatusBadRequest, err.Error())
			return
		}
		overrides = ov
	}

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		slog.Error("begin template allocation tx", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after a successful Commit
	qtx := h.q.WithTx(tx)

	if req.GlobalDefaultIDs != nil {
		if err := qtx.DeleteSharedTemplateAllocations(ctx); err != nil {
			slog.Error("clear shared template allocations", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
		for _, id := range globalIDs {
			if err := qtx.InsertSharedTemplateAllocation(ctx, id); err != nil {
				slog.Error("insert shared template allocation", "error", err)
				httpx.Error(w, http.StatusInternalServerError, "internal error")
				return
			}
		}
	}
	if req.MyOverrides != nil {
		if err := qtx.DeleteUserTemplateAllocations(ctx, pgconv.UUID(actor.ID)); err != nil {
			slog.Error("clear user template allocations", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
		for _, o := range overrides {
			if err := qtx.InsertUserTemplateAllocation(ctx, store.InsertUserTemplateAllocationParams{
				TemplateID: o.id, UserID: pgconv.UUID(actor.ID), Enabled: o.enabled,
			}); err != nil {
				slog.Error("insert user template allocation", "error", err)
				httpx.Error(w, http.StatusInternalServerError, "internal error")
				return
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		slog.Error("commit template allocation tx", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}

	dto, ok := h.loadTemplateAllocations(w, ctx, actor)
	if !ok {
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"templates": dto})
}

// resolvedOverride is a validated overlay decision (a visible template + on/off).
type resolvedOverride struct {
	id      uuid.UUID
	enabled bool
}

// resolveGlobalDefaultable parses each id and verifies the template exists, is
// visible to actor, and is a builtin/global (a user template can never be a
// global default). Duplicates fold. An unknown/invisible id is reported
// generically so a probe cannot enumerate other users' private template ids.
func (h *Handler) resolveGlobalDefaultable(ctx context.Context, actor store.User, ids []string) ([]uuid.UUID, error) {
	seen := map[uuid.UUID]struct{}{}
	out := make([]uuid.UUID, 0, len(ids))
	for _, raw := range ids {
		id, err := uuid.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid template id %q", raw)
		}
		if _, dup := seen[id]; dup {
			continue
		}
		t, err := h.templateForViewer(ctx, actor, id)
		if err != nil {
			return nil, err
		}
		if t.Scope != "builtin" && t.Scope != "global" {
			return nil, errors.New("only builtin or global templates can be global defaults")
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out, nil
}

// resolveOverrides parses each override, verifies the template is visible to
// actor, and folds duplicates (last decision wins). An unknown/invisible id is
// reported generically (no private-id enumeration).
func (h *Handler) resolveOverrides(ctx context.Context, actor store.User, in []templateOverrideInput) ([]resolvedOverride, error) {
	byID := map[uuid.UUID]bool{}
	order := make([]uuid.UUID, 0, len(in))
	for _, o := range in {
		id, err := uuid.Parse(o.TemplateID)
		if err != nil {
			return nil, fmt.Errorf("invalid template id %q", o.TemplateID)
		}
		if _, err := h.templateForViewer(ctx, actor, id); err != nil {
			return nil, err
		}
		if _, seen := byID[id]; !seen {
			order = append(order, id)
		}
		byID[id] = o.Enabled
	}
	out := make([]resolvedOverride, 0, len(order))
	for _, id := range order {
		out = append(out, resolvedOverride{id: id, enabled: byID[id]})
	}
	return out, nil
}

// templateForViewer fetches a template through the visibility filter, mapping an
// invisible/absent id to a generic not-allocatable error.
func (h *Handler) templateForViewer(ctx context.Context, actor store.User, id uuid.UUID) (store.AgentTemplate, error) {
	t, err := h.q.GetAgentTemplateForViewer(ctx, store.GetAgentTemplateForViewerParams{
		ID:       id,
		IsAdmin:  actor.IsAdmin,
		ViewerID: pgconv.UUID(actor.ID),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.AgentTemplate{}, errors.New("one or more templates are not allocatable")
		}
		return store.AgentTemplate{}, err
	}
	return t, nil
}

// loadTemplateAllocations builds the caller's allocation view. On a store error
// it writes a 500 and returns ok=false.
func (h *Handler) loadTemplateAllocations(w http.ResponseWriter, ctx context.Context, actor store.User) ([]templateAllocationDTO, bool) {
	rows, err := h.q.ListTemplateAllocationsForViewer(ctx, store.ListTemplateAllocationsForViewerParams{
		IsAdmin:  actor.IsAdmin,
		ViewerID: pgconv.UUID(actor.ID),
	})
	if err != nil {
		slog.Error("list template allocations", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return nil, false
	}
	out := make([]templateAllocationDTO, 0, len(rows))
	for _, row := range rows {
		var myOverride *bool
		effective := row.GlobalDefault
		if row.MyOverride.Valid {
			v := row.MyOverride.Bool
			myOverride = &v
			effective = v
		}
		out = append(out, templateAllocationDTO{
			ID:            row.ID.String(),
			Name:          row.Name,
			Description:   row.Description,
			Scope:         row.Scope,
			IsBuiltin:     row.IsBuiltin,
			GlobalDefault: row.GlobalDefault,
			MyOverride:    myOverride,
			Effective:     effective,
		})
	}
	return out, true
}
