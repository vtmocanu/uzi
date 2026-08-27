package handler

import (
	"context"
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

	"github.com/vtmocanu/uzi/api/internal/agenttmpl"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/termsafe"
)

// nameRe is the kebab-case constraint on a template name: the subagent identity
// (filename and PRD #4 routing key), so it is validated on create and immutable
// after. It moved to agenttmpl because PRD #37's roster + selection validation
// (workersvc) must hold repo-detected agents to the very same rule; a second copy
// here would let one surface accept an identity the other rejects.
var nameRe = agenttmpl.NameRe

// fullTokenRe matches a high-confidence, complete Anthropic token. The server
// rejects only this (a real credential pasted into a template); the UI does the
// looser, non-blocking warning so prompts that merely mention the token format
// stay legal. The middle segment plus a long token body avoids matching a bare
// "sk-ant-..." mentioned in prose.
var fullTokenRe = regexp.MustCompile(`sk-ant-[A-Za-z0-9]+-[A-Za-z0-9_-]{40,}`)

// leadNameRe mirrors the worker's LEAD_NAME_RE (agent/src/agents.ts): a template
// whose name matches it is routed to the main thread as the lead, not registered
// as an invokable subagent. The single legitimate lead is the seeded builtin, so
// the API refuses to create/rename any non-builtin (global or user) template into
// a lead name (Decision 8). That guarantees a claim payload can carry at most one
// lead-matching template — the worker-side pin — regardless of allocation. Case-
// insensitive to match the worker regex, though nameRe already forces lowercase.
// Shared from agenttmpl with PRD #37, which drops the lead from the `own` roster's
// selectable subagents for exactly this reason.
var leadNameRe = agenttmpl.LeadNameRe

// agentTemplateWriteStore is the narrow slice of *store.Queries the builtin-
// definition and reset handlers touch (issue #223). Deliberately NOT achieved by
// widening Handler.q to an interface (which would have to enumerate every query the
// package uses), so these two handlers' DB touch can be faked without a live
// database. *store.Queries satisfies it.
type agentTemplateWriteStore interface {
	GetAgentTemplate(ctx context.Context, id uuid.UUID) (store.AgentTemplate, error)
	ResetBuiltinAgentTemplate(ctx context.Context, arg store.ResetBuiltinAgentTemplateParams) (store.AgentTemplate, error)
}

// agentTemplateDTO is the safe JSON view of a template row. tools is null when
// the template inherits all tools (matching the render semantics); model is
// null when it inherits the model.
type agentTemplateDTO struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Model       *string  `json:"model"`
	Tools       []string `json:"tools"`
	PromptBody  string   `json:"prompt_body"`
	IsBuiltin   bool     `json:"is_builtin"`
	// Scope is builtin|global|user (PRD #18 M6). IsBuiltin is retained as a compat
	// flag kept in lockstep with Scope=='builtin' by builtin_scope_ck; the UI reads
	// Scope for badges and the "my agents" grouping. UserID is set only for
	// scope='user' rows (the owner).
	Scope     string  `json:"scope"`
	UserID    *string `json:"user_id"`
	UpdatedBy *string `json:"updated_by"`
	// Origin is the store's scope-aware provenance for a builtin row (PRD #602):
	// "embedded" (shipped body, pristine or reset), "synced" (last written by the
	// agent-source sync), or "admin" (an admin edit). It is a pointer because a
	// non-builtin row carries a NULL origin (store.AgentTemplate.Origin is a
	// nullable pgtype.Text), which serializes as JSON null. The web keys the
	// provenance badge on it (M5).
	Origin *string `json:"origin"`
	// DiffersFromBuiltin is COMPUTED per request and stored nowhere (issue #201
	// M4a): whether this row's content still matches the definition this binary
	// ships under the same name. See differsFromBuiltin for what "content" means
	// and for the three ways it is false.
	DiffersFromBuiltin bool      `json:"differs_from_builtin"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

func templateDTO(t store.AgentTemplate) agentTemplateDTO {
	dto := agentTemplateDTO{
		ID:                 t.ID.String(),
		Name:               t.Name,
		Description:        t.Description,
		Tools:              decodeTools(t.Tools),
		PromptBody:         t.PromptBody,
		IsBuiltin:          t.IsBuiltin,
		Scope:              t.Scope,
		DiffersFromBuiltin: differsFromBuiltin(t),
		CreatedAt:          t.CreatedAt.Time,
		UpdatedAt:          t.UpdatedAt.Time,
	}
	if t.Model.Valid {
		dto.Model = &t.Model.String
	}
	if t.UserID.Valid {
		u := uuid.UUID(t.UserID.Bytes).String()
		dto.UserID = &u
	}
	if t.UpdatedBy.Valid {
		s := uuid.UUID(t.UpdatedBy.Bytes).String()
		dto.UpdatedBy = &s
	}
	if t.Origin.Valid {
		dto.Origin = &t.Origin.String
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

// fieldsToDefinition builds a comparison Definition from a validated write,
// mirroring templateToDefinition so the customized-flag decision uses the same
// normalization the drift badge does (decodeTools canonicalizes tools; a blank
// model is inherit).
func fieldsToDefinition(name string, f validatedFields) agenttmpl.Definition {
	d := agenttmpl.Definition{
		Name:        name,
		Description: f.description,
		Tools:       decodeTools(f.tools),
		PromptBody:  f.promptBody,
	}
	if f.model.Valid {
		d.Model = f.model.String
	}
	return d
}

// updateMarksCustomized reports the customized flag an UpdateAgentTemplate write
// should persist (issue #339). A builtin row whose submitted content matches the
// shipped definition byte-for-byte (per agenttmpl.SameContent) is stored
// customized=false, so "save the shipped body" is idempotent with Reset and the
// row keeps tracking future shipped changes via RefreshPristineBuiltin (PRD #275,
// issue #201). Every other write — a genuine edit, a non-builtin row (no shipped
// definition to track), or a removed builtin (no embedded def) — marks it
// customized. This is the logical complement of differsFromBuiltin for a builtin
// that ships a definition; a removed builtin (no embedded def) is the one case
// where the two agree rather than invert (both leave the row marked customized).
func updateMarksCustomized(t store.AgentTemplate, f validatedFields) bool {
	if t.Scope != "builtin" {
		return true
	}
	def, ok := agenttmpl.BuiltinByName(t.Name)
	if !ok {
		return true
	}
	return !agenttmpl.SameContent(fieldsToDefinition(t.Name, f), def)
}

// differsFromBuiltin reports whether a stored row's content has drifted from the
// definition THIS BINARY ships under the same name (issue #201 M4a). It is
// computed on every read and stored nowhere: there is no column, no hash and no
// migration behind it, so it can never go stale against the shipped corpus.
//
// It is false in three distinct situations, and the third conflates two states
// with opposite Reset outcomes:
//
//  1. The row is not a builtin. A global template, or a user template whose name
//     merely COLLIDES with a builtin (00048 explicitly allows a user to own a
//     'coder' beside the builtin one), has no shipped counterpart to differ from.
//     Keying on name alone would badge that user's private row and advertise a
//     Reset that answers 400 "only builtin templates can be reset".
//  2. The row is a builtin and matches the shipped definition.
//  3. The row is a builtin with NO shipped definition — a builtin removed from a
//     later release. Nothing to compare against, so it reports false even though
//     Reset would answer 409. That distinction reaches the UI through
//     GET /agent-templates/{id}/builtin rather than through a tri-state here.
//
// The scope check reads `scope` rather than `is_builtin`; 00048's
// `CHECK (is_builtin = (scope = 'builtin'))` makes the two a provable
// biconditional, so this is a style choice and no fixture can tell them apart.
//
// templateToDefinition supplies the normalization — it is the existing mapping of
// a stored row onto the very type BuiltinByName returns, so both sides of the
// comparison are agenttmpl.Definition and the jsonb tools column is decoded, not
// byte-compared. Do NOT add drift-specific behaviour to it: it is on the
// /rendered export path that writes into an agent workspace.
func differsFromBuiltin(t store.AgentTemplate) bool {
	if t.Scope != "builtin" {
		return false
	}
	def, ok := agenttmpl.BuiltinByName(t.Name)
	if !ok {
		return false
	}
	return !agenttmpl.SameContent(templateToDefinition(t), def)
}

// builtinDefinitionDTO is the JSON view of a shipped builtin definition served by
// GetBuiltinAgentTemplate. Field names and null semantics mirror
// agentTemplateDTO's (model null = inherit, tools null = inherit all) so the
// editor diffs like against like; the row-only fields (id, scope, timestamps)
// have no meaning for a definition that lives in the binary.
type builtinDefinitionDTO struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Model       *string  `json:"model"`
	Tools       []string `json:"tools"`
	PromptBody  string   `json:"prompt_body"`
}

func builtinDefinitionView(def agenttmpl.Definition) builtinDefinitionDTO {
	dto := builtinDefinitionDTO{
		Name:        def.Name,
		Description: def.Description,
		Tools:       def.Tools,
		PromptBody:  def.PromptBody,
	}
	if def.Model != "" {
		m := def.Model
		dto.Model = &m
	}
	return dto
}

// GetBuiltinAgentTemplate serves the definition this binary ships for a builtin
// row, so the editor can show a shipped-vs-stored diff BEFORE anyone presses
// Reset — the destructive act the diff exists to make safe. Until this route
// existed, BuiltinByName had one non-test caller (ResetAgentTemplate), so the
// shipped body reached a client only AFTER it had already overwritten the row.
//
// Read-only and additive: nothing here writes. It is a sub-resource of {id},
// matching the /{id}/rendered precedent, which also keeps the ~44 KB shipped
// corpus out of the list response a nested DTO field would have put it on.
func (h *Handler) GetBuiltinAgentTemplate(w http.ResponseWriter, r *http.Request) {
	actor, t, ok := h.loadTemplateForWrite(w, r)
	if !ok {
		return
	}
	writeBuiltinDefinition(w, actor, t)
}

// writeBuiltinDefinition is the whole of GetBuiltinAgentTemplate below the row
// fetch, split out so the status matrix is exercisable without a database.
//
// Authorization mirrors ResetAgentTemplate exactly, including its ordering: the
// row is loaded unfiltered and authorized FIRST, so a template the caller may not
// see returns 404 rather than a 400/409 that would confirm the id exists. The
// gate is the WRITE gate on purpose — this endpoint exists to make Reset safe to
// press, and its audience is exactly the callers who can press it.
//
// The two refusals reuse ResetAgentTemplate's semantics rather than inventing
// new ones: a non-builtin row has no shipped definition (400, as reset answers),
// and a builtin whose definition this release no longer ships is the 409 case
// reset already names. That 409 is also how the UI learns not to offer Reset for
// a removed builtin — a state differs_from_builtin reports as false.
func writeBuiltinDefinition(w http.ResponseWriter, actor store.User, t store.AgentTemplate) {
	if status, ok := authorizeTemplateWrite(actor, t); !ok {
		httpx.Error(w, status, templateWriteDenyMessage(status))
		return
	}
	if !t.IsBuiltin {
		httpx.Error(w, http.StatusBadRequest, "only builtin templates have a shipped definition")
		return
	}
	def, ok := agenttmpl.BuiltinByName(t.Name)
	if !ok {
		httpx.Error(w, http.StatusConflict, "no builtin definition to reset to")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"builtin": builtinDefinitionView(def)})
}

// templateWriteRequest is the create/update body. name and scope are only read
// on create (both immutable afterwards); is_builtin, user_id and timestamps are
// server-controlled and never accepted from the client.
type templateWriteRequest struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Model       *string  `json:"model"`
	Tools       []string `json:"tools"`
	PromptBody  string   `json:"prompt_body"`
	Scope       string   `json:"scope"`
}

// authorizeTemplateWrite decides whether actor may edit/delete t and, if not,
// which status to return. Mirrors authorizeSkillWrite: builtin and global are
// admin-only; a user template is owner-only. A user-scope row the actor neither
// owns nor (as admin) can edit is reported as 404 so a private template's
// existence never leaks to a non-owner; an admin who can see it but may not edit
// it gets 403 (honest, not a leak). ok=true carries status 0.
func authorizeTemplateWrite(actor store.User, t store.AgentTemplate) (int, bool) {
	switch t.Scope {
	case "builtin", "global":
		if actor.IsAdmin {
			return 0, true
		}
		return http.StatusForbidden, false
	case "user":
		if t.UserID.Valid && uuid.UUID(t.UserID.Bytes) == actor.ID {
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

// templateWriteDenyMessage maps an authz status to a user-facing message. 404 is
// worded as not-found (the existence-hiding case), 403 as a permission denial.
func templateWriteDenyMessage(status int) string {
	if status == http.StatusNotFound {
		return "template not found"
	}
	return "you do not have permission to modify this template"
}

// ListAgentTemplates returns the templates visible to the caller: builtin ∪
// global ∪ the caller's own user templates (admins see all scopes). Deliberately
// NOT an all-shared read — that would leak private user templates (PRD #18 M6).
func (h *Handler) ListAgentTemplates(w http.ResponseWriter, r *http.Request) {
	actor, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	rows, err := h.q.ListAgentTemplatesForViewer(r.Context(), store.ListAgentTemplatesForViewerParams{
		IsAdmin:  actor.IsAdmin,
		ViewerID: pgUUID(actor.ID),
	})
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

// GetAgentTemplate returns one template by id, subject to the same visibility
// rule as the list (404 when the caller may not see it).
func (h *Handler) GetAgentTemplate(w http.ResponseWriter, r *http.Request) {
	t, ok := h.loadTemplateForViewer(w, r)
	if !ok {
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"template": templateDTO(t)})
}

// GetRenderedAgentTemplate returns the canonical Claude Code subagent Markdown
// for a template (any authenticated user). PRD #4 writes this straight into an
// agent workspace, so it is served as raw Markdown, not a JSON envelope.
func (h *Handler) GetRenderedAgentTemplate(w http.ResponseWriter, r *http.Request) {
	t, ok := h.loadTemplateForViewer(w, r)
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

// CreateAgentTemplate creates a global (admin) or user (owner) template. Builtin
// scope is never creatable via the API (builtins are seeded); name must be
// kebab-case, may not be a reserved lead name (Decision 8), and is immutable
// after creation.
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
	// The lead is the seeded builtin only: no API-created template (global or
	// user) may take a lead name, so a claim can never carry two lead-matching
	// templates (Decision 8; the worker routes the first by array order).
	if leadNameRe.MatchString(name) {
		httpx.Error(w, http.StatusBadRequest, "name is reserved for the built-in lead orchestrator")
		return
	}

	// Blank scope defaults to 'global' so the pre-M6 admin create (which sent no
	// scope field) keeps its exact behavior — a global, admin-only template. The
	// "my agents" flow (M7) sends scope='user' explicitly.
	scope := req.Scope
	if scope == "" {
		scope = "global"
	}
	var userID pgtype.UUID
	switch scope {
	case "global":
		if !actor.IsAdmin {
			httpx.Error(w, http.StatusForbidden, "only admins can create global templates")
			return
		}
	case "user":
		userID = pgUUID(actor.ID)
	default:
		httpx.Error(w, http.StatusBadRequest, "scope must be 'global' or 'user'")
		return
	}

	fields, err := validateTemplateFields(req)
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx := r.Context()
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		slog.Error("begin create template tx", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after a successful Commit
	qtx := h.q.WithTx(tx)

	row, err := qtx.CreateAgentTemplate(ctx, store.CreateAgentTemplateParams{
		Name:        name,
		Description: fields.description,
		Model:       fields.model,
		Tools:       fields.tools,
		PromptBody:  fields.promptBody,
		Scope:       scope,
		UserID:      userID,
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
	// A new global template is a global default from creation (removable via the
	// allocation UI). No empty-means-all cliff (PRD #18 M7). User templates are
	// delivered only via the owner's own overlay, so they get no default row.
	if scope == "global" {
		if err := qtx.InsertSharedTemplateAllocation(ctx, row.ID); err != nil {
			slog.Error("seed global template allocation", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
	}
	if err := tx.Commit(ctx); err != nil {
		slog.Error("commit create template tx", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusCreated, map[string]any{"template": templateDTO(row)})
}

// UpdateAgentTemplate edits a template's mutable fields. name and scope are
// immutable and ignored if present. Authorization is scope-based (see
// authorizeTemplateWrite): builtin/global admin-only, user owner-only.
func (h *Handler) UpdateAgentTemplate(w http.ResponseWriter, r *http.Request) {
	actor, t, ok := h.loadTemplateForWrite(w, r)
	if !ok {
		return
	}
	if status, ok := authorizeTemplateWrite(actor, t); !ok {
		httpx.Error(w, status, templateWriteDenyMessage(status))
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
		ID:          t.ID,
		Description: fields.description,
		Model:       fields.model,
		Tools:       fields.tools,
		PromptBody:  fields.promptBody,
		UpdatedBy:   pgUUID(actor.ID),
		Customized:  updateMarksCustomized(t, fields),
	})
	if err != nil {
		slog.Error("update agent template", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"template": templateDTO(row)})
}

// DeleteAgentTemplate deletes a global (admin) or user (owner) template. Builtins
// return 409 — they get reset instead, so PRD #4 can rely on them existing.
func (h *Handler) DeleteAgentTemplate(w http.ResponseWriter, r *http.Request) {
	actor, t, ok := h.loadTemplateForWrite(w, r)
	if !ok {
		return
	}
	if t.IsBuiltin {
		httpx.Error(w, http.StatusConflict, "builtin templates cannot be deleted; reset them instead")
		return
	}
	if status, ok := authorizeTemplateWrite(actor, t); !ok {
		httpx.Error(w, status, templateWriteDenyMessage(status))
		return
	}
	if _, err := h.q.DeleteAgentTemplate(r.Context(), t.ID); err != nil {
		slog.Error("delete agent template", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ResetAgentTemplate re-applies a builtin's embedded definition. Builtins are
// admin-only (authorizeTemplateWrite), so this is effectively admin-only for the
// rows it can act on; non-builtins have no default to reset to and return 400.
func (h *Handler) ResetAgentTemplate(w http.ResponseWriter, r *http.Request) {
	actor, t, ok := h.loadTemplateForWrite(w, r)
	if !ok {
		return
	}
	// Authorize BEFORE the builtin-only rule: a template the caller may not see
	// (another user's private template) must return 404, not a 400 that would
	// confirm the id exists — an existence oracle. Consistent with Update/Delete.
	if status, ok := authorizeTemplateWrite(actor, t); !ok {
		httpx.Error(w, status, templateWriteDenyMessage(status))
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
	// Reset via the dedicated query, not UpdateAgentTemplate: a reset returns the
	// row to pristine (customized=false) so it resumes tracking upstream shipped
	// changes on future boots (PRD #275 M4b/D2), whereas UpdateAgentTemplate marks
	// the row customized.
	row, err := h.templateWriteStore().ResetBuiltinAgentTemplate(r.Context(), store.ResetBuiltinAgentTemplateParams{
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

// loadTemplateForViewer resolves the {id} path param, requires auth, and fetches
// the row through the visibility filter (a template the caller may not see is
// 404). Used by the read handlers. Returns the row.
//
// It deliberately does NOT return the actor, unlike loadTemplateForWrite: the
// visibility filter has already applied it, so a read handler has nothing left to
// decide and both callers discarded it. (Dropped while landing issue #201 M4a —
// unparam had flagged it in the backlog for a while, and the lint ratchet's
// whole-files rule surfaced it the moment this file was touched.)
func (h *Handler) loadTemplateForViewer(w http.ResponseWriter, r *http.Request) (store.AgentTemplate, bool) {
	actor, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return store.AgentTemplate{}, false
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid template id")
		return store.AgentTemplate{}, false
	}
	t, err := h.q.GetAgentTemplateForViewer(r.Context(), store.GetAgentTemplateForViewerParams{
		ID:       id,
		IsAdmin:  actor.IsAdmin,
		ViewerID: pgUUID(actor.ID),
	})
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

// loadTemplateForWrite resolves the {id} path param, requires auth, and fetches
// the row unfiltered (the write handlers apply scope-based authz next). A missing
// id is 404. Returns the actor and row.
func (h *Handler) loadTemplateForWrite(w http.ResponseWriter, r *http.Request) (store.User, store.AgentTemplate, bool) {
	actor, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return store.User{}, store.AgentTemplate{}, false
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid template id")
		return store.User{}, store.AgentTemplate{}, false
	}
	t, err := h.templateWriteStore().GetAgentTemplate(r.Context(), id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Error(w, http.StatusNotFound, "template not found")
			return store.User{}, store.AgentTemplate{}, false
		}
		slog.Error("get agent template", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return store.User{}, store.AgentTemplate{}, false
	}
	return actor, t, true
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
	//
	// BEHAVIOUR CHANGE, #166: the description is validated by termsafe.Validate,
	// which rejects unicode.Cf format characters (zero-width joiners, bidi
	// overrides, the BOM) on top of the control characters hasControlChar already
	// caught. A builtin/global template description renders into ANOTHER user's
	// tooltip (AgentPicker title=), so a bidi override or ZWJ here crosses a
	// principal boundary — this is the same rule, and the same reject-not-strip
	// gate, that the w.name fix (workers.go) applied for the admin fleet list. A
	// field is exempt from this only if it is rendered EXCLUSIVELY to the principal
	// who authored it, which a shared-scope description is not. Consequence: a
	// description carrying a Cf-joined sequence, such as the ZWJ in a family emoji
	// (👨‍👩‍👧), now 400s. Intended and narrow — plain emoji and VS16 hearts are
	// unaffected — and it is a refusal, never a silent rewrite of author input.
	if err := termsafe.Validate("description", f.description); err != nil {
		return f, err
	}

	f.promptBody = req.PromptBody
	if strings.TrimSpace(f.promptBody) == "" {
		return f, errors.New("prompt body must not be empty")
	}

	model, err := validateModel(req.Model)
	if err != nil {
		return f, err
	}
	f.model = model

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

// validateModel is the thin HTTP-facing wrapper over agenttmpl.ValidateModel
// (the single source of the Decision 4 rules), shared by the template write path
// (validateTemplateFields) and the per-user default-model endpoint. It maps the
// neutral (string, error) core onto the storage type: a nil pointer or a blank
// value becomes NULL (inherit); a valid value becomes a set pgtype.Text.
func validateModel(raw *string) (pgtype.Text, error) {
	if raw == nil {
		return pgtype.Text{}, nil
	}
	m, err := agenttmpl.ValidateModel(*raw)
	if err != nil {
		return pgtype.Text{}, err
	}
	if m == "" {
		return pgtype.Text{}, nil
	}
	return pgtype.Text{String: m, Valid: true}, nil
}

// validateEffort is the thin HTTP-facing wrapper over agenttmpl.ValidateEffort
// (the single source of the closed-enum effort rules, PRD #617), used by the
// per-user default-effort endpoint. A nil pointer or a blank value becomes NULL
// (inherit — we then omit the SDK effort key, so the SDK default `high` applies);
// a valid level becomes a set pgtype.Text; anything else is an error (mapped to
// 400 by the caller).
func validateEffort(raw *string) (pgtype.Text, error) {
	if raw == nil {
		return pgtype.Text{}, nil
	}
	e, err := agenttmpl.ValidateEffort(*raw)
	if err != nil {
		return pgtype.Text{}, err
	}
	if e == "" {
		return pgtype.Text{}, nil
	}
	return pgtype.Text{String: e, Valid: true}, nil
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

// pgUUID wraps a uuid you KNOW IS PRESENT as a valid pgtype.UUID. It sets Valid=true
// unconditionally — including for uuid.Nil, which becomes the REAL all-zero uuid, not SQL
// NULL. Do not "fix" that: at call sites that legitimately assume presence, auto-NULLing
// would turn a loud FK violation into a silent NULL write.
//
// The trap is passing uuid.Nil as an "absent" sentinel to a sqlc.narg parameter — the
// query's `IS NULL` branch never fires, so the filter matches nothing and the endpoint
// SILENTLY RETURNS NOTHING instead of erroring. For a genuinely optional id, leave the
// zero pgtype.UUID (Valid:false → NULL) and call this only on the present branch, as the
// optional-parent paths in this file already do.
//
// Two sibling copies exist (workersvc/service.go, selfimprove/engine.go) with the same
// contract; keep these comments in step.
func pgUUID(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: true}
}

// isUniqueViolation reports whether err is a Postgres unique-constraint failure
// (SQLSTATE 23505).
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
