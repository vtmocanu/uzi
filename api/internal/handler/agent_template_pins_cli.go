package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sort"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Reason codes of the two 500s PUT /api/me/agent-templates/{id}/model-pins distinguishes from a
// plain "internal error" (the same contract as PUT /api/me/model-settings).
const (
	reasonTemplateCommitUnknown       = "template_commit_unknown"
	reasonTemplateSavedReadbackFailed = "template_saved_readback_failed"
	reasonTemplateNotEditable         = "template_not_editable"
	msgTemplateCommitUnknown          = "the save may or may not have been applied; check with `uzi agent-template show`"
	msgTemplateSavedReadbackFailed    = "template pins saved; reading them back failed"
	msgTemplateNotEditable            = "builtin and global templates are edited in the web UI by an admin"
)

// cliTemplateDTO is the restricted template view of the CLI pin routes: identity, the two pins
// and two flags. It carries no prompt body, description or tools. Model and CodexModel are null
// when the template inherits.
type cliTemplateDTO struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	Scope      string  `json:"scope"`
	Model      *string `json:"model"`
	CodexModel *string `json:"codex_model"`
	Customized bool    `json:"customized"`
	// Editable is true only for the caller's own scope='user' rows, the only rows the pin route
	// writes.
	Editable bool `json:"editable"`
}

func cliTemplateView(t store.AgentTemplate, caller uuid.UUID) cliTemplateDTO {
	dto := cliTemplateDTO{
		ID:         t.ID.String(),
		Name:       t.Name,
		Scope:      t.Scope,
		Customized: t.Customized,
		Editable:   ownsUserTemplate(t, caller),
	}
	if t.Model.Valid {
		dto.Model = &t.Model.String
	}
	if t.CodexModel.Valid {
		dto.CodexModel = &t.CodexModel.String
	}
	return dto
}

func ownsUserTemplate(t store.AgentTemplate, caller uuid.UUID) bool {
	return t.Scope == "user" && t.UserID.Valid && uuid.UUID(t.UserID.Bytes) == caller
}

// ListMyAgentTemplates serves GET /api/me/agent-templates: builtin, global and the caller's own
// user templates in the restricted CLI view. IsAdmin is hard-coded false: a uza_ token reaches
// handlers with IsAdmin=true, and the admin branch of ListAgentTemplatesForViewer would list
// every user's private templates.
func (h *Handler) ListMyAgentTemplates(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	rows, err := h.q.ListAgentTemplatesForViewer(r.Context(), store.ListAgentTemplatesForViewerParams{
		IsAdmin:  false,
		ViewerID: pgconv.UUID(user.ID),
	})
	if err != nil {
		slog.Error("list my agent templates", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]cliTemplateDTO, 0, len(rows))
	for _, t := range rows {
		out = append(out, cliTemplateView(t, user.ID))
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"templates": out})
}

// PutMyAgentTemplateModelPins serves PUT /api/me/agent-templates/{id}/model-pins (PRD #3264 M3):
// set or clear (null) the Claude and Codex model pins of the caller's OWN user template, in one
// transaction under the row lock, touching nothing else.
//
// Order: bounded strict body read (its error is revealed only after authorization, so a
// template the caller may not see answers 404 whatever the body); lock the row; authorize;
// validate; write; commit; read back.
//
// Authorization is deliberately NOT authorizeTemplateWrite: a uza_ admin_ro token reaches
// handlers with IsAdmin=true, and that function would let it edit builtin and global rows.
// This route has no admin branch: builtin and global are 403 for everyone, another user's
// template is 404.
func (h *Handler) PutMyAgentTemplateModelPins(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	id, ok := httpx.PathUUID(w, r, "id", "template")
	if !ok {
		return
	}
	var req map[string]json.RawMessage
	decodeErr := httpx.DecodeJSONStrict(r, &req)

	ctx := r.Context()
	begin := h.templatePinsBeginTx
	if begin == nil {
		if h.pool == nil {
			slog.Error("begin template pins tx", "error", "no pool")
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
		begin = h.pool.Begin
	}
	tx, err := begin(ctx)
	if err != nil {
		slog.Error("begin template pins tx", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	qtx := h.q.WithTx(tx)

	t, err := qtx.GetAgentTemplateForUpdate(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Error(w, http.StatusNotFound, "template not found")
			return
		}
		slog.Error("lock agent template", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	switch t.Scope {
	case "user":
		if !ownsUserTemplate(t, user.ID) {
			httpx.Error(w, http.StatusNotFound, "template not found")
			return
		}
	default:
		httpx.ErrorReason(w, http.StatusForbidden, msgTemplateNotEditable, reasonTemplateNotEditable)
		return
	}

	if decodeErr != nil || req == nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	keys := make([]string, 0, len(req))
	for k := range req {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if k != "model" && k != "codex_model" {
			writeModelSettingsFieldError(w, k, "unknown field "+k)
			return
		}
	}
	if len(req) == 0 {
		httpx.Error(w, http.StatusBadRequest, "at least one of model, codex_model is required")
		return
	}

	model, codexModel := t.Model, t.CodexModel
	lanes := []struct {
		field, harness string
		dst            *pgtype.Text
	}{
		{"model", "claude", &model},
		{"codex_model", "codex", &codexModel},
	}
	for _, l := range lanes {
		raw, present := req[l.field]
		if !present {
			continue
		}
		var v *string
		if err := json.Unmarshal(raw, &v); err != nil {
			writeModelSettingsFieldError(w, l.field, l.field+": must be a string or null")
			return
		}
		val, err := validateTemplatePin(l.harness, l.field, v)
		if err != nil {
			writeModelSettingsFieldError(w, l.field, err.Error())
			return
		}
		*l.dst = val
	}

	if _, err := qtx.UpdateAgentTemplateModelPins(ctx, store.UpdateAgentTemplateModelPinsParams{
		ID:         t.ID,
		Model:      model,
		CodexModel: codexModel,
		UpdatedBy:  pgconv.UUID(user.ID),
	}); err != nil {
		slog.Error("update template model pins", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		slog.Error("template pins commit outcome unknown", "error", err, "template", t.ID)
		httpx.ErrorReason(w, http.StatusInternalServerError, msgTemplateCommitUnknown, reasonTemplateCommitUnknown)
		return
	}

	read := h.templatePinsReadback
	if read == nil {
		read = h.q.GetAgentTemplate
	}
	row, err := read(context.WithoutCancel(ctx), t.ID)
	if err != nil {
		slog.Error("template pins readback", "error", err, "template", t.ID)
		httpx.ErrorReason(w, http.StatusInternalServerError, msgTemplateSavedReadbackFailed, reasonTemplateSavedReadbackFailed)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"template": cliTemplateView(row, user.ID)})
}
