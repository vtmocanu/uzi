package handler

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/httpx"
	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/skilltmpl"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// skillDTO is the JSON view of a skill row. body is included (unlike a secret):
// it is the user-authored playbook content, editable in the UI. user_id/updated_by
// are null except for user-scope / edited rows.
type skillDTO struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Body        string    `json:"body"`
	Scope       string    `json:"scope"`
	UserID      *string   `json:"user_id"`
	UpdatedBy   *string   `json:"updated_by"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func skillToDTO(s store.Skill) skillDTO {
	dto := skillDTO{
		ID:          s.ID.String(),
		Name:        s.Name,
		Description: s.Description,
		Body:        s.Body,
		Scope:       s.Scope,
		CreatedAt:   s.CreatedAt.Time,
		UpdatedAt:   s.UpdatedAt.Time,
	}
	if s.UserID.Valid {
		u := uuid.UUID(s.UserID.Bytes).String()
		dto.UserID = &u
	}
	if s.UpdatedBy.Valid {
		u := uuid.UUID(s.UpdatedBy.Bytes).String()
		dto.UpdatedBy = &u
	}
	return dto
}

// skillWriteRequest is the create/update body. name and scope are read only on
// create (both immutable afterwards); user_id/updated_by/timestamps are
// server-controlled and never accepted from the client.
type skillWriteRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Body        string `json:"body"`
	Scope       string `json:"scope"`
}

// validatedSkill holds the sanitized column values for a skill write.
type validatedSkill struct {
	description string
	body        string
}

// validateSkillFields enforces the save-time rules shared by create and update:
// a non-empty single-line description (it renders on one synthesized frontmatter
// line, so a newline/control char could forge or break out of the YAML), a
// non-empty body within SKILL_MAX_BYTES, and the same high-confidence
// full-token guardrail the agent templates use (a skill body is user content
// delivered to workers and logged around — a pasted credential must not ride
// along). The per-run count cap (SKILLS_MAX_PER_RUN) is NOT checked here; it
// spans all templates and is enforced at claim assembly (M3).
func validateSkillFields(req skillWriteRequest, maxBytes int) (validatedSkill, error) {
	var f validatedSkill

	f.description = strings.TrimSpace(req.Description)
	if f.description == "" {
		return f, errors.New("description must not be empty")
	}
	if hasControlChar(f.description) {
		return f, errors.New("description must not contain newlines or control characters")
	}

	f.body = req.Body
	if strings.TrimSpace(f.body) == "" {
		return f, errors.New("body must not be empty")
	}
	if len(f.body) > maxBytes {
		return f, errors.New("body exceeds the maximum allowed size")
	}

	if fullTokenRe.MatchString(f.description) || fullTokenRe.MatchString(f.body) {
		return f, errors.New("skill appears to contain a full Anthropic token; remove the credential")
	}
	return f, nil
}

// authorizeSkillWrite decides whether actor may edit/delete s and, if not, which
// status to return. Builtin and global are admin-only; a user skill is
// owner-only. A user-scope row the actor neither owns nor (as admin) can see is
// reported as 404 so a private skill's existence never leaks to a non-owner;
// an admin who can see it but may not edit it gets 403 (honest, not a leak).
// ok=true carries status 0.
func authorizeSkillWrite(actor store.User, s store.Skill) (int, bool) {
	switch s.Scope {
	case "builtin", "global":
		if actor.IsAdmin {
			return 0, true
		}
		return http.StatusForbidden, false
	case "user":
		if s.UserID.Valid && uuid.UUID(s.UserID.Bytes) == actor.ID {
			return 0, true
		}
		if actor.IsAdmin {
			return http.StatusForbidden, false
		}
		return http.StatusNotFound, false
	default:
		return http.StatusForbidden, false
	}
}

// resetSkillStatus decides the outcome of a reset for (actor, skill): it applies
// the write authz FIRST (so a skill the caller may not see returns 404, hiding
// existence), and only then the builtin-only rule (400 for a skill the caller can
// see but that has no embedded default). ok=true carries status 0.
func resetSkillStatus(actor store.User, s store.Skill) (int, bool) {
	if status, ok := authorizeSkillWrite(actor, s); !ok {
		return status, false
	}
	if s.Scope != "builtin" {
		return http.StatusBadRequest, false
	}
	return 0, true
}

// resetSkillDenyMessage maps a reset-denial status to a user-facing message: the
// builtin-only 400 has its own wording; 403/404 reuse the shared write messages.
func resetSkillDenyMessage(status int) string {
	if status == http.StatusBadRequest {
		return "only builtin skills can be reset"
	}
	return skillWriteDenyMessage(status)
}

// ListSkills returns every skill visible to the caller: builtin ∪ global ∪ the
// caller's own user skills. Admins see all scopes. This is deliberately NOT the
// agent-templates all-shared read — that would leak private user skills.
func (h *Handler) ListSkills(w http.ResponseWriter, r *http.Request) {
	actor, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	rows, err := h.q.ListSkillsForViewer(r.Context(), store.ListSkillsForViewerParams{
		IsAdmin:  actor.IsAdmin,
		ViewerID: pgUUID(actor.ID),
	})
	if err != nil {
		slog.Error("list skills", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]skillDTO, 0, len(rows))
	for _, s := range rows {
		out = append(out, skillToDTO(s))
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"skills": out})
}

// GetSkill returns one skill by id, subject to the same visibility rule as the
// list (404 when the caller may not see it).
func (h *Handler) GetSkill(w http.ResponseWriter, r *http.Request) {
	actor, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid skill id")
		return
	}
	s, err := h.q.GetSkillForViewer(r.Context(), store.GetSkillForViewerParams{
		ID:       id,
		IsAdmin:  actor.IsAdmin,
		ViewerID: pgUUID(actor.ID),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Error(w, http.StatusNotFound, "skill not found")
			return
		}
		slog.Error("get skill", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"skill": skillToDTO(s)})
}

// CreateSkill creates a global (admin) or user (owner) skill. Builtin scope is
// never creatable via the API (builtins are seeded); name must be kebab-case and
// is immutable after creation.
func (h *Handler) CreateSkill(w http.ResponseWriter, r *http.Request) {
	actor, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var req skillWriteRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}

	name := strings.TrimSpace(req.Name)
	if !skilltmpl.NameRe.MatchString(name) {
		httpx.Error(w, http.StatusBadRequest, "name must be kebab-case (lowercase letters, digits, hyphens; max 64 chars)")
		return
	}

	var userID pgtype.UUID
	switch req.Scope {
	case "global":
		if !actor.IsAdmin {
			httpx.Error(w, http.StatusForbidden, "only admins can create global skills")
			return
		}
	case "user":
		userID = pgUUID(actor.ID)
	default:
		httpx.Error(w, http.StatusBadRequest, "scope must be 'global' or 'user'")
		return
	}

	fields, err := validateSkillFields(req, h.cfg.SkillMaxBytes)
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return
	}

	row, err := h.q.CreateSkill(r.Context(), store.CreateSkillParams{
		Name:        name,
		Description: fields.description,
		Body:        fields.body,
		Scope:       req.Scope,
		UserID:      userID,
		UpdatedBy:   pgUUID(actor.ID),
	})
	if err != nil {
		if isUniqueViolation(err) {
			httpx.Error(w, http.StatusConflict, "a skill with that name already exists")
			return
		}
		slog.Error("create skill", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusCreated, map[string]any{"skill": skillToDTO(row)})
}

// UpdateSkill edits a skill's mutable fields (description, body). name and scope
// are immutable and ignored if present. Authorization is scope-based (see
// authorizeSkillWrite): builtin/global admin-only, user owner-only.
func (h *Handler) UpdateSkill(w http.ResponseWriter, r *http.Request) {
	actor, s, ok := h.loadSkillForWrite(w, r)
	if !ok {
		return
	}
	if status, ok := authorizeSkillWrite(actor, s); !ok {
		httpx.Error(w, status, skillWriteDenyMessage(status))
		return
	}
	var req skillWriteRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	fields, err := validateSkillFields(req, h.cfg.SkillMaxBytes)
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	row, err := h.q.UpdateSkill(r.Context(), store.UpdateSkillParams{
		ID:          s.ID,
		Description: fields.description,
		Body:        fields.body,
		UpdatedBy:   pgUUID(actor.ID),
	})
	if err != nil {
		slog.Error("update skill", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"skill": skillToDTO(row)})
}

// DeleteSkill deletes a global (admin) or user (owner) skill. Builtins return
// 409 — they are reset, never deleted, so allocations to them can rely on them
// existing.
func (h *Handler) DeleteSkill(w http.ResponseWriter, r *http.Request) {
	actor, s, ok := h.loadSkillForWrite(w, r)
	if !ok {
		return
	}
	if s.Scope == "builtin" {
		httpx.Error(w, http.StatusConflict, "builtin skills cannot be deleted; reset them instead")
		return
	}
	if status, ok := authorizeSkillWrite(actor, s); !ok {
		httpx.Error(w, status, skillWriteDenyMessage(status))
		return
	}
	if _, err := h.q.DeleteSkill(r.Context(), s.ID); err != nil {
		slog.Error("delete skill", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ResetSkill re-applies a builtin skill's embedded definition (admin only).
// Non-builtins have no default to reset to and return 400.
func (h *Handler) ResetSkill(w http.ResponseWriter, r *http.Request) {
	actor, s, ok := h.loadSkillForWrite(w, r)
	if !ok {
		return
	}
	// Authorize BEFORE the builtin-only rule: a skill the caller may not see
	// (another user's private skill) must return 404, not a 400 that would confirm
	// the id exists — an existence oracle. Consistent with Update/Delete.
	if status, ok := resetSkillStatus(actor, s); !ok {
		httpx.Error(w, status, resetSkillDenyMessage(status))
		return
	}
	def, ok := skilltmpl.BuiltinByName(s.Name)
	if !ok {
		httpx.Error(w, http.StatusConflict, "no builtin definition to reset to")
		return
	}
	row, err := h.q.UpdateSkill(r.Context(), store.UpdateSkillParams{
		ID:          s.ID,
		Description: def.Description,
		Body:        def.Body,
		UpdatedBy:   pgUUID(actor.ID),
	})
	if err != nil {
		slog.Error("reset skill", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"skill": skillToDTO(row)})
}

// loadSkillForWrite resolves the {id} path param, requires auth, and fetches the
// row unfiltered (the write handlers apply scope-based authz next). A missing id
// is 404. It returns the actor and row.
func (h *Handler) loadSkillForWrite(w http.ResponseWriter, r *http.Request) (store.User, store.Skill, bool) {
	actor, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return store.User{}, store.Skill{}, false
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid skill id")
		return store.User{}, store.Skill{}, false
	}
	s, err := h.q.GetSkill(r.Context(), id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Error(w, http.StatusNotFound, "skill not found")
			return store.User{}, store.Skill{}, false
		}
		slog.Error("get skill", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return store.User{}, store.Skill{}, false
	}
	return actor, s, true
}

// skillWriteDenyMessage maps an authz status to a user-facing message. 404 is
// worded as not-found (the existence-hiding case), 403 as a permission denial.
func skillWriteDenyMessage(status int) string {
	if status == http.StatusNotFound {
		return "skill not found"
	}
	return "you do not have permission to modify this skill"
}
