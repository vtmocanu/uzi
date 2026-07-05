package handler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"gitlab.example.com/vtmocanu/uzi/api/internal/httpx"
	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// allocatedSkillDTO is one skill allocated to a template, in the caller's view.
type allocatedSkillDTO struct {
	SkillID     string `json:"skill_id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Scope       string `json:"scope"`
}

// templateSkillsDTO splits a template's allocations the caller may see into the
// shared (admin-managed) rows and the caller's own overlay rows. Other users'
// overlays are never included — not even for an admin.
type templateSkillsDTO struct {
	Shared []allocatedSkillDTO `json:"shared"`
	Mine   []allocatedSkillDTO `json:"mine"`
}

// allocationsWriteRequest is the replace-set body. A nil slice pointer means
// "leave this half untouched"; a non-nil empty slice means "clear this half".
// The two halves are independently authorized: shared is admin-only, mine is any
// authenticated user (their own overlay).
type allocationsWriteRequest struct {
	SharedSkillIDs *[]string `json:"shared_skill_ids"`
	MySkillIDs     *[]string `json:"my_skill_ids"`
}

// allocatableAsShared reports whether a skill may be a shared allocation: only
// builtin/global skills (a shared row must reference a skill every user can see).
func allocatableAsShared(s store.Skill) bool {
	return s.Scope == "builtin" || s.Scope == "global"
}

// allocatableAsMine reports whether actor may allocate s to their own overlay:
// any builtin/global skill, or actor's own user skill.
func allocatableAsMine(s store.Skill, actor store.User) bool {
	if s.Scope == "builtin" || s.Scope == "global" {
		return true
	}
	return s.Scope == "user" && s.UserID.Valid && uuid.UUID(s.UserID.Bytes) == actor.ID
}

// GetTemplateSkills returns the skills allocated to a template that the caller
// may see: the shared rows plus the caller's own overlay rows only.
func (h *Handler) GetTemplateSkills(w http.ResponseWriter, r *http.Request) {
	actor, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	tid, ok := h.templateIDForSkills(w, r)
	if !ok {
		return
	}
	dto, ok := h.loadTemplateSkills(w, r.Context(), tid, actor.ID)
	if !ok {
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"allocations": dto})
}

// SetTemplateSkills replaces a template's shared and/or mine allocation sets.
// Providing shared_skill_ids requires admin and may reference only builtin/global
// skills; my_skill_ids is any user's own overlay and may reference builtin/global
// or the caller's own user skills. Each provided half is fully replaced.
func (h *Handler) SetTemplateSkills(w http.ResponseWriter, r *http.Request) {
	actor, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	tid, ok := h.templateIDForSkills(w, r)
	if !ok {
		return
	}
	var req allocationsWriteRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.SharedSkillIDs == nil && req.MySkillIDs == nil {
		httpx.Error(w, http.StatusBadRequest, "provide shared_skill_ids and/or my_skill_ids")
		return
	}

	ctx := r.Context()
	var sharedIDs, myIDs []uuid.UUID
	if req.SharedSkillIDs != nil {
		if !actor.IsAdmin {
			httpx.Error(w, http.StatusForbidden, "only admins can set shared skill allocations")
			return
		}
		ids, err := h.resolveAllocatable(ctx, *req.SharedSkillIDs, func(s store.Skill) bool { return allocatableAsShared(s) })
		if err != nil {
			httpx.Error(w, http.StatusBadRequest, err.Error())
			return
		}
		sharedIDs = ids
	}
	if req.MySkillIDs != nil {
		ids, err := h.resolveAllocatable(ctx, *req.MySkillIDs, func(s store.Skill) bool { return allocatableAsMine(s, actor) })
		if err != nil {
			httpx.Error(w, http.StatusBadRequest, err.Error())
			return
		}
		myIDs = ids
	}

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		slog.Error("begin allocation tx", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after a successful Commit
	qtx := h.q.WithTx(tx)

	if req.SharedSkillIDs != nil {
		if err := qtx.DeleteSharedAllocations(ctx, tid); err != nil {
			slog.Error("clear shared allocations", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
		for _, sid := range sharedIDs {
			if err := qtx.InsertSharedAllocation(ctx, store.InsertSharedAllocationParams{TemplateID: tid, SkillID: sid}); err != nil {
				slog.Error("insert shared allocation", "error", err)
				httpx.Error(w, http.StatusInternalServerError, "internal error")
				return
			}
		}
	}
	if req.MySkillIDs != nil {
		if err := qtx.DeleteUserAllocations(ctx, store.DeleteUserAllocationsParams{TemplateID: tid, UserID: pgUUID(actor.ID)}); err != nil {
			slog.Error("clear user allocations", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
		for _, sid := range myIDs {
			if err := qtx.InsertUserAllocation(ctx, store.InsertUserAllocationParams{TemplateID: tid, SkillID: sid, UserID: pgUUID(actor.ID)}); err != nil {
				slog.Error("insert user allocation", "error", err)
				httpx.Error(w, http.StatusInternalServerError, "internal error")
				return
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		slog.Error("commit allocation tx", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}

	dto, ok := h.loadTemplateSkills(w, ctx, tid, actor.ID)
	if !ok {
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"allocations": dto})
}

// templateIDForSkills parses the {id} path param and verifies the template
// exists (allocations reference a real template). It writes the error response
// and returns ok=false on any failure.
func (h *Handler) templateIDForSkills(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid template id")
		return uuid.UUID{}, false
	}
	if _, err := h.q.GetAgentTemplate(r.Context(), id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Error(w, http.StatusNotFound, "template not found")
			return uuid.UUID{}, false
		}
		slog.Error("get template for skills", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return uuid.UUID{}, false
	}
	return id, true
}

// resolveAllocatable parses each id and verifies the referenced skill exists and
// passes allow (the scope/ownership rule for this allocation half). Duplicate ids
// are folded. Any bad id or disallowed skill yields an error the caller maps to
// 400; an unknown skill is reported generically so a probe cannot enumerate
// other users' private skill ids.
func (h *Handler) resolveAllocatable(ctx context.Context, ids []string, allow func(store.Skill) bool) ([]uuid.UUID, error) {
	seen := map[uuid.UUID]struct{}{}
	out := make([]uuid.UUID, 0, len(ids))
	for _, raw := range ids {
		sid, err := uuid.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid skill id %q", raw)
		}
		if _, dup := seen[sid]; dup {
			continue
		}
		s, err := h.q.GetSkill(ctx, sid)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, errors.New("one or more skills are not allocatable")
			}
			return nil, err
		}
		if !allow(s) {
			return nil, errors.New("one or more skills are not allocatable")
		}
		seen[sid] = struct{}{}
		out = append(out, sid)
	}
	return out, nil
}

// loadTemplateSkills builds the caller's allocation view for a template. On a
// store error it writes a 500 and returns ok=false.
func (h *Handler) loadTemplateSkills(w http.ResponseWriter, ctx context.Context, tid, viewerID uuid.UUID) (templateSkillsDTO, bool) {
	rows, err := h.q.ListAllocationsForTemplateForViewer(ctx, store.ListAllocationsForTemplateForViewerParams{
		TemplateID: tid,
		ViewerID:   pgUUID(viewerID),
	})
	if err != nil {
		slog.Error("list template allocations", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return templateSkillsDTO{}, false
	}
	dto := templateSkillsDTO{Shared: []allocatedSkillDTO{}, Mine: []allocatedSkillDTO{}}
	for _, row := range rows {
		item := allocatedSkillDTO{
			SkillID:     row.SkillID.String(),
			Name:        row.SkillName,
			Description: row.SkillDescription,
			Scope:       row.SkillScope,
		}
		if row.UserID.Valid {
			dto.Mine = append(dto.Mine, item)
		} else {
			dto.Shared = append(dto.Shared, item)
		}
	}
	return dto, true
}
