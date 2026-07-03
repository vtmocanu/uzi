package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/agenttmpl"
	"gitlab.example.com/vtmocanu/uzi/api/internal/httpx"
	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// nameRe is the kebab-case constraint on a template name: the subagent identity
// (filename and PRD #4 routing key), so it is validated on create and immutable
// after.
var nameRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// fullTokenRe matches a high-confidence, complete Anthropic token. The server
// rejects only this (a real credential pasted into a template); the UI does the
// looser, non-blocking warning so prompts that merely mention the token format
// stay legal. The middle segment plus a long token body avoids matching a bare
// "sk-ant-..." mentioned in prose.
var fullTokenRe = regexp.MustCompile(`sk-ant-[A-Za-z0-9]+-[A-Za-z0-9_-]{40,}`)

// agentTemplateDTO is the safe JSON view of a template row. tools is null when
// the template inherits all tools (matching the render semantics); model is
// null when it inherits the model.
type agentTemplateDTO struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Model       *string   `json:"model"`
	Tools       []string  `json:"tools"`
	PromptBody  string    `json:"prompt_body"`
	IsBuiltin   bool      `json:"is_builtin"`
	UpdatedBy   *string   `json:"updated_by"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func templateDTO(t store.AgentTemplate) agentTemplateDTO {
	dto := agentTemplateDTO{
		ID:          t.ID.String(),
		Name:        t.Name,
		Description: t.Description,
		Tools:       decodeTools(t.Tools),
		PromptBody:  t.PromptBody,
		IsBuiltin:   t.IsBuiltin,
		CreatedAt:   t.CreatedAt.Time,
		UpdatedAt:   t.UpdatedAt.Time,
	}
	if t.Model.Valid {
		dto.Model = &t.Model.String
	}
	if t.UpdatedBy.Valid {
		s := uuid.UUID(t.UpdatedBy.Bytes).String()
		dto.UpdatedBy = &s
	}
	return dto
}

// decodeTools turns the stored jsonb allowlist into a slice. A NULL/empty
// column means inherit-all and yields nil (serialized as JSON null). A decode
// error should be impossible (writes validate the shape); it is logged and
// treated as inherit-all rather than failing the read.
func decodeTools(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil {
		slog.Error("decode template tools", "error", err)
		return nil
	}
	return out
}

// templateToDefinition builds the renderer input from a stored row.
func templateToDefinition(t store.AgentTemplate) agenttmpl.Definition {
	d := agenttmpl.Definition{
		Name:        t.Name,
		Description: t.Description,
		Tools:       decodeTools(t.Tools),
		PromptBody:  t.PromptBody,
	}
	if t.Model.Valid {
		d.Model = t.Model.String
	}
	return d
}

// templateWriteRequest is the admin-supplied body for create/update. name is
// only read on create (immutable afterwards); is_builtin and timestamps are
// server-controlled and never accepted from the client.
type templateWriteRequest struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Model       *string  `json:"model"`
	Tools       []string `json:"tools"`
	PromptBody  string   `json:"prompt_body"`
}

// ListAgentTemplates returns every template (any authenticated user).
func (h *Handler) ListAgentTemplates(w http.ResponseWriter, r *http.Request) {
	rows, err := h.q.ListAgentTemplates(r.Context())
	if err != nil {
		slog.Error("list agent templates", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]agentTemplateDTO, 0, len(rows))
	for _, t := range rows {
		out = append(out, templateDTO(t))
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"templates": out})
}

// GetAgentTemplate returns one template by id (any authenticated user).
func (h *Handler) GetAgentTemplate(w http.ResponseWriter, r *http.Request) {
	t, ok := h.loadTemplate(w, r)
	if !ok {
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"template": templateDTO(t)})
}

// GetRenderedAgentTemplate returns the canonical Claude Code subagent Markdown
// for a template (any authenticated user). PRD #4 writes this straight into an
// agent workspace, so it is served as raw Markdown, not a JSON envelope.
func (h *Handler) GetRenderedAgentTemplate(w http.ResponseWriter, r *http.Request) {
	t, ok := h.loadTemplate(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	// Defense in depth: this is raw user-editable content served inline, so stop
	// the browser from MIME-sniffing it into something executable.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(agenttmpl.Render(templateToDefinition(t))); err != nil {
		slog.Error("write rendered template", "error", err)
	}
}

// CreateAgentTemplate creates a new (non-builtin) template (admin only).
func (h *Handler) CreateAgentTemplate(w http.ResponseWriter, r *http.Request) {
	actor, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var req templateWriteRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}

	name := strings.TrimSpace(req.Name)
	if !nameRe.MatchString(name) {
		httpx.Error(w, http.StatusBadRequest, "name must be kebab-case (lowercase letters, digits, single hyphens)")
		return
	}
	fields, err := validateTemplateFields(req)
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return
	}

	row, err := h.q.CreateAgentTemplate(r.Context(), store.CreateAgentTemplateParams{
		Name:        name,
		Description: fields.description,
		Model:       fields.model,
		Tools:       fields.tools,
		PromptBody:  fields.promptBody,
		UpdatedBy:   pgUUID(actor.ID),
	})
	if err != nil {
		if isUniqueViolation(err) {
			httpx.Error(w, http.StatusConflict, "a template with that name already exists")
			return
		}
		slog.Error("create agent template", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusCreated, map[string]any{"template": templateDTO(row)})
}

// UpdateAgentTemplate edits a template's mutable fields (admin only). name is
// immutable and ignored if present in the body.
func (h *Handler) UpdateAgentTemplate(w http.ResponseWriter, r *http.Request) {
	actor, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid template id")
		return
	}
	var req templateWriteRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	fields, err := validateTemplateFields(req)
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return
	}

	row, err := h.q.UpdateAgentTemplate(r.Context(), store.UpdateAgentTemplateParams{
		ID:          id,
		Description: fields.description,
		Model:       fields.model,
		Tools:       fields.tools,
		PromptBody:  fields.promptBody,
		UpdatedBy:   pgUUID(actor.ID),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Error(w, http.StatusNotFound, "template not found")
			return
		}
		slog.Error("update agent template", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"template": templateDTO(row)})
}

// DeleteAgentTemplate deletes a non-builtin template (admin only). Builtins
// return 409 — they get reset instead, so PRD #4 can rely on them existing.
func (h *Handler) DeleteAgentTemplate(w http.ResponseWriter, r *http.Request) {
	t, ok := h.loadTemplate(w, r)
	if !ok {
		return
	}
	if t.IsBuiltin {
		httpx.Error(w, http.StatusConflict, "builtin templates cannot be deleted; reset them instead")
		return
	}
	if _, err := h.q.DeleteAgentTemplate(r.Context(), t.ID); err != nil {
		slog.Error("delete agent template", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ResetAgentTemplate re-applies a builtin's embedded definition (admin only).
// Non-builtins have no default to reset to and return 400.
func (h *Handler) ResetAgentTemplate(w http.ResponseWriter, r *http.Request) {
	actor, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	t, ok := h.loadTemplate(w, r)
	if !ok {
		return
	}
	if !t.IsBuiltin {
		httpx.Error(w, http.StatusBadRequest, "only builtin templates can be reset")
		return
	}
	def, ok := agenttmpl.BuiltinByName(t.Name)
	if !ok {
		// A builtin row with no embedded definition: a removed builtin. Nothing
		// to reset to.
		httpx.Error(w, http.StatusConflict, "no builtin definition to reset to")
		return
	}
	model, tools, err := storeColumns(def)
	if err != nil {
		slog.Error("encode builtin for reset", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	row, err := h.q.UpdateAgentTemplate(r.Context(), store.UpdateAgentTemplateParams{
		ID:          t.ID,
		Description: def.Description,
		Model:       model,
		Tools:       tools,
		PromptBody:  def.PromptBody,
		UpdatedBy:   pgUUID(actor.ID),
	})
	if err != nil {
		slog.Error("reset agent template", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"template": templateDTO(row)})
}

// loadTemplate resolves the {id} path param and fetches the row, writing the
// appropriate error response and returning ok=false on any failure.
func (h *Handler) loadTemplate(w http.ResponseWriter, r *http.Request) (store.AgentTemplate, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid template id")
		return store.AgentTemplate{}, false
	}
	t, err := h.q.GetAgentTemplate(r.Context(), id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Error(w, http.StatusNotFound, "template not found")
			return store.AgentTemplate{}, false
		}
		slog.Error("get agent template", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return store.AgentTemplate{}, false
	}
	return t, true
}

// validatedFields holds the sanitized column values for a write.
type validatedFields struct {
	description string
	model       pgtype.Text
	tools       []byte
	promptBody  string
}

// validateTemplateFields enforces the server-side rules shared by create and
// update: non-empty description and prompt body, model NULL-or-non-empty, tools
// NULL-or-array-of-non-empty-strings, and the high-confidence secret guardrail.
func validateTemplateFields(req templateWriteRequest) (validatedFields, error) {
	var f validatedFields

	f.description = strings.TrimSpace(req.Description)
	if f.description == "" {
		return f, errors.New("description must not be empty")
	}
	// The frontmatter fields (description, model, each tool name) each render on
	// a single YAML line, so a newline/CR/control char would forge or duplicate
	// a key or break out of the block, making the rendered subagent file diverge
	// from the structured columns PRD #4 trusts. The prompt body renders after
	// the frontmatter, so its newlines are legitimate Markdown and are allowed.
	if hasControlChar(f.description) {
		return f, errors.New("description must not contain newlines or control characters")
	}

	f.promptBody = req.PromptBody
	if strings.TrimSpace(f.promptBody) == "" {
		return f, errors.New("prompt body must not be empty")
	}

	if req.Model != nil {
		m := strings.TrimSpace(*req.Model)
		if m != "" {
			if hasControlChar(m) {
				return f, errors.New("model must not contain newlines or control characters")
			}
			f.model = pgtype.Text{String: m, Valid: true}
		}
	}

	// An empty (or absent) tools list normalizes to NULL = inherit all, so a
	// direct-API empty array does not persist a `[]` that renders as inherit-all
	// but lists as "none".
	if len(req.Tools) > 0 {
		for _, t := range req.Tools {
			if strings.TrimSpace(t) == "" {
				return f, errors.New("tools must be a list of non-empty names")
			}
			// A comma is the tools-line separator; a control char breaks the
			// line. Either would let a tool name inject extra entries into the
			// rendered allowlist.
			if hasControlChar(t) || strings.Contains(t, ",") {
				return f, errors.New("tool names must not contain commas, newlines, or control characters")
			}
		}
		b, err := json.Marshal(req.Tools)
		if err != nil {
			return f, errors.New("invalid tools list")
		}
		f.tools = b
	}

	if fullTokenRe.MatchString(f.description) || fullTokenRe.MatchString(f.promptBody) {
		return f, errors.New("template appears to contain a full Anthropic token; remove the credential")
	}
	return f, nil
}

// storeColumns converts a builtin definition into the model/tools column types
// used on write.
func storeColumns(def agenttmpl.Definition) (pgtype.Text, []byte, error) {
	var model pgtype.Text
	if def.Model != "" {
		model = pgtype.Text{String: def.Model, Valid: true}
	}
	var tools []byte
	if len(def.Tools) > 0 {
		b, err := json.Marshal(def.Tools)
		if err != nil {
			return pgtype.Text{}, nil, err
		}
		tools = b
	}
	return model, tools, nil
}

// hasControlChar reports whether s contains a rune that would break out of a
// single frontmatter line: a newline, carriage return, any other control
// character, or the Unicode replacement character (malformed UTF-8). A plain
// space is not a control character, so ordinary multi-word text is unaffected.
func hasControlChar(s string) bool {
	for _, r := range s {
		if r == unicode.ReplacementChar || unicode.IsControl(r) {
			return true
		}
	}
	return false
}

// pgUUID wraps a google/uuid value as a valid pgtype.UUID.
func pgUUID(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: true}
}

// isUniqueViolation reports whether err is a Postgres unique-constraint failure
// (SQLSTATE 23505).
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
