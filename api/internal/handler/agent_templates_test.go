package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestLeadNameReserved pins Decision 8: no API-created template (global or user)
// may take a lead name, so a claim can never carry two lead-matching templates.
// Case-insensitive and anchored, matching the worker's LEAD_NAME_RE.
func TestLeadNameReserved(t *testing.T) {
	reserved := []string{"lead", "orchestrator", "LEAD", "Orchestrator", "Lead", "ORCHESTRATOR"}
	for _, s := range reserved {
		if !leadNameRe.MatchString(s) {
			t.Errorf("leadNameRe must reserve %q", s)
		}
	}
	// Anchored: only the exact words are reserved, not names that merely contain them.
	allowed := []string{"coder", "reviewer", "lead-helper", "my-lead", "orchestrators", "leader"}
	for _, s := range allowed {
		if leadNameRe.MatchString(s) {
			t.Errorf("leadNameRe must allow %q", s)
		}
	}
}

// TestAuthorizeTemplateWrite is the core write-authz matrix mirroring skills:
// builtin/global are admin-only, a user template is owner-only, and a non-owner
// non-admin sees 404 (existence hidden) while an admin who cannot edit a user
// template sees 403.
func TestAuthorizeTemplateWrite(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	other := store.User{ID: uuid.New()}
	admin := store.User{ID: uuid.New(), IsAdmin: true}

	builtin := store.AgentTemplate{Scope: "builtin", IsBuiltin: true}
	global := store.AgentTemplate{Scope: "global"}
	mine := store.AgentTemplate{Scope: "user", UserID: pgUUID(owner.ID)}

	cases := []struct {
		name       string
		actor      store.User
		tmpl       store.AgentTemplate
		wantOK     bool
		wantStatus int
	}{
		{"admin edits builtin", admin, builtin, true, 0},
		{"user edits builtin", owner, builtin, false, 403},
		{"admin edits global", admin, global, true, 0},
		{"user edits global", owner, global, false, 403},
		{"owner edits own user template", owner, mine, true, 0},
		{"admin edits others user template", admin, mine, false, 403},
		{"other user edits user template", other, mine, false, 404},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, ok := authorizeTemplateWrite(c.actor, c.tmpl)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if !ok && status != c.wantStatus {
				t.Errorf("status = %d, want %d", status, c.wantStatus)
			}
		})
	}
}

func TestNameRe(t *testing.T) {
	valid := []string{"coder", "fact-checker", "a", "a1", "spec-keeper", "x-y-z"}
	for _, s := range valid {
		if !nameRe.MatchString(s) {
			t.Errorf("nameRe rejected valid name %q", s)
		}
	}
	invalid := []string{"", "Coder", "-x", "x-", "x--y", "x_y", "x y", "café", "UPPER"}
	for _, s := range invalid {
		if nameRe.MatchString(s) {
			t.Errorf("nameRe accepted invalid name %q", s)
		}
	}
}

func TestValidateTemplateFields(t *testing.T) {
	base := templateWriteRequest{Description: "does a thing.", PromptBody: "Do the thing.\n"}

	// Happy path with an explicit tools list and model.
	m := "opus"
	ok := base
	ok.Model = &m
	ok.Tools = []string{"Bash", "Read"}
	f, err := validateTemplateFields(ok)
	if err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
	if !f.model.Valid || f.model.String != "opus" {
		t.Errorf("model not carried through: %+v", f.model)
	}
	var gotTools []string
	if err := json.Unmarshal(f.tools, &gotTools); err != nil {
		t.Fatalf("tools not valid json: %v", err)
	}
	if len(gotTools) != 2 || gotTools[0] != "Bash" {
		t.Errorf("tools = %v, want [Bash Read]", gotTools)
	}

	// model omitted -> NULL (inherit); tools omitted -> nil (inherit all).
	f, err = validateTemplateFields(base)
	if err != nil {
		t.Fatalf("minimal request rejected: %v", err)
	}
	if f.model.Valid {
		t.Error("absent model should be NULL")
	}
	if f.tools != nil {
		t.Error("absent tools should be nil")
	}

	// An explicit empty tools array normalizes to NULL (inherit all), not a
	// stored `[]` that would list as "none".
	emptyTools := base
	emptyTools.Tools = []string{}
	if f, err = validateTemplateFields(emptyTools); err != nil || f.tools != nil {
		t.Errorf("empty tools array should normalize to NULL: tools=%v err=%v", f.tools, err)
	}

	// Empty model string trims to NULL, not a blank model.
	empty := ""
	blankModel := base
	blankModel.Model = &empty
	if f, err = validateTemplateFields(blankModel); err != nil || f.model.Valid {
		t.Errorf("blank model should become NULL: valid=%v err=%v", f.model.Valid, err)
	}

	rejects := map[string]templateWriteRequest{
		"empty description": {Description: "  ", PromptBody: "body\n"},
		"empty prompt body": {Description: "d.", PromptBody: "   "},
		"blank tool name":   {Description: "d.", PromptBody: "b\n", Tools: []string{"Bash", ""}},
	}
	for name, req := range rejects {
		if _, err := validateTemplateFields(req); err == nil {
			t.Errorf("%s: expected rejection", name)
		}
	}
}

// TestValidateModel covers only the handler wrapper's storage mapping onto
// pgtype.Text; the Decision 4 rule cases live with the shared core in
// agenttmpl.TestValidateModel (single source, no drift).
func TestValidateModel(t *testing.T) {
	strptr := func(s string) *string { return &s }

	// nil pointer and blank string both map to NULL (inherit), no error.
	for _, raw := range []*string{nil, strptr(""), strptr("   ")} {
		got, err := validateModel(raw)
		if err != nil {
			t.Errorf("validateModel(%v) errored: %v", raw, err)
		}
		if got.Valid {
			t.Errorf("validateModel(%v) = %q, want NULL (inherit)", raw, got.String)
		}
	}

	// A valid value becomes a set pgtype.Text, trimmed.
	got, err := validateModel(strptr("  sonnet  "))
	if err != nil {
		t.Fatalf("valid model rejected: %v", err)
	}
	if !got.Valid || got.String != "sonnet" {
		t.Errorf("validateModel = (%q, valid=%v), want %q", got.String, got.Valid, "sonnet")
	}

	// An invalid value propagates the core error (rule coverage is in agenttmpl).
	if _, err := validateModel(strptr("claude 3")); err == nil {
		t.Error("expected rejection for a model with interior whitespace")
	}
}

func TestRejectFrontmatterInjection(t *testing.T) {
	strptr := func(s string) *string { return &s }
	injections := map[string]templateWriteRequest{
		"newline in description":         {Description: "legit\ntools: Bash, Write, Edit", PromptBody: "b\n"},
		"cr in description":              {Description: "legit\rmodel: opus", PromptBody: "b\n"},
		"delimiter break in description": {Description: "x\n---\nname: evil", PromptBody: "b\n"},
		"tab in description":             {Description: "a\tb", PromptBody: "b\n"},
		"newline in model":               {Description: "d.", PromptBody: "b\n", Model: strptr("opus\ntools: Write")},
		"newline in tool":                {Description: "d.", PromptBody: "b\n", Tools: []string{"Bash", "Read\ntools: Write"}},
		"comma in tool":                  {Description: "d.", PromptBody: "b\n", Tools: []string{"Bash, Write"}},
	}
	for name, req := range injections {
		if _, err := validateTemplateFields(req); err == nil {
			t.Errorf("%s: expected rejection", name)
		}
	}

	// A newline in the prompt body is legitimate Markdown and must be allowed.
	if _, err := validateTemplateFields(templateWriteRequest{
		Description: "d.",
		PromptBody:  "line one\nline two\n",
	}); err != nil {
		t.Errorf("multiline prompt body should be allowed, got: %v", err)
	}
}

func TestSecretGuardrailRejectsFullToken(t *testing.T) {
	fullToken := "sk-ant-api03-" + strings.Repeat("A", 80)

	// A real full token in the prompt body is rejected.
	if _, err := validateTemplateFields(templateWriteRequest{
		Description: "leaks a key.",
		PromptBody:  "Use this key: " + fullToken + "\n",
	}); err == nil {
		t.Error("full token in prompt body should be rejected")
	}
	// ...and in the description.
	if _, err := validateTemplateFields(templateWriteRequest{
		Description: "key is " + fullToken,
		PromptBody:  "body\n",
	}); err == nil {
		t.Error("full token in description should be rejected")
	}

	// A prompt that merely mentions the token FORMAT stays legal (no false
	// positive): the server guardrail only trips on a high-confidence full token.
	if _, err := validateTemplateFields(templateWriteRequest{
		Description: "explains tokens.",
		PromptBody:  "Anthropic tokens start with sk-ant- and are pasted in Settings.\n",
	}); err != nil {
		t.Errorf("format mention should be allowed, got: %v", err)
	}
}

// fakeTemplateViewerDB is a store.DBTX for the GetAgentTemplateForViewer QueryRow
// pass-through guards (sites GetAgentTemplate, resolveOverrides→templateForViewer, and
// GetTemplateSkills→templateIDForSkills). Like fakeSkillDB it CAPTURES the raw
// positional QueryRow args so a test can see WHICH identity params the handler built.
//
// When tmpl != nil, QueryRow scans back that template so the handler continues; when
// nil it reports pgx.ErrNoRows (the not-visible path). Query returns an empty-but-valid
// result for handlers that go on to list allocations (GetTemplateSkills).
type fakeTemplateViewerDB struct {
	tmpl    *store.AgentTemplate
	gotArgs []any
	called  bool
}

func (*fakeTemplateViewerDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (*fakeTemplateViewerDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return &emptyViewerRows{}, nil
}
func (f *fakeTemplateViewerDB) QueryRow(_ context.Context, _ string, args ...any) pgx.Row {
	f.called = true
	f.gotArgs = args
	if f.tmpl == nil {
		return fakeScanRow{func(...any) error { return pgx.ErrNoRows }}
	}
	tt := *f.tmpl
	// GetAgentTemplateForViewer scans 13 columns: id, name, description, model, tools,
	// prompt_body, is_builtin, updated_by, created_at, updated_at, scope, user_id,
	// customized (agent_templates.sql.go). id (dest[0]) and scope (dest[10]) are all the
	// read path needs to proceed cleanly; the rest keep their zero values.
	return fakeScanRow{func(dest ...any) error {
		if p, ok := dest[0].(*uuid.UUID); ok {
			*p = tt.ID
		}
		if p, ok := dest[10].(*string); ok {
			*p = tt.Scope
		}
		return nil
	}}
}

// templateViewerArgs unpacks GetAgentTemplateForViewer's positional args
// (id, is_admin, viewer_id — agent_templates.sql.go). The t.Fatalf type guards keep the
// non-admin assertion from going vacuous on a mis-shaped capture; the admin `== true`
// subtest is the backstop no zero value can satisfy (see skillQueryArgs).
func templateViewerArgs(t *testing.T, db *fakeTemplateViewerDB) (uuid.UUID, bool, pgtype.UUID) {
	t.Helper()
	if !db.called {
		t.Fatal("GetAgentTemplateForViewer was never queried")
	}
	if len(db.gotArgs) != 3 {
		t.Fatalf("query got %d args, want 3 (id, is_admin, viewer_id): %#v", len(db.gotArgs), db.gotArgs)
	}
	id, ok := db.gotArgs[0].(uuid.UUID)
	if !ok {
		t.Fatalf("arg 0 (id) = %#v, want uuid.UUID", db.gotArgs[0])
	}
	isAdmin, ok := db.gotArgs[1].(bool)
	if !ok {
		t.Fatalf("arg 1 (is_admin) = %#v, want bool", db.gotArgs[1])
	}
	viewer, ok := db.gotArgs[2].(pgtype.UUID)
	if !ok {
		t.Fatalf("arg 2 (viewer_id) = %#v, want pgtype.UUID", db.gotArgs[2])
	}
	return id, isAdmin, viewer
}

// listAgentTemplatesRec drives ListAgentTemplates (GET, no path param) as actor.
func listAgentTemplatesRec(t *testing.T, db store.DBTX, actor store.User) *httptest.ResponseRecorder {
	t.Helper()
	h := &Handler{q: store.New(db)}
	req := httptest.NewRequest(http.MethodGet, "/api/agent-templates", nil)
	req = req.WithContext(mw.ContextWithUser(req.Context(), actor))
	rec := httptest.NewRecorder()
	h.ListAgentTemplates(rec, req)
	return rec
}

// TestListAgentTemplatesPassesCallerIdentity pins the caller-identity pass-through into
// ListAgentTemplatesForViewer (agent_templates.go:342). Mutating `IsAdmin: actor.IsAdmin`
// to `IsAdmin: true` makes every caller list as admin — a total visibility bypass that
// leaks every private user template — and it survived a full-scope `go test ./...` run.
// Both flag directions are asserted; the admin `== true` check is the backstop.
func TestListAgentTemplatesPassesCallerIdentity(t *testing.T) {
	t.Run("non-admin caller", func(t *testing.T) {
		actor := nonAdminCaller()
		db := &fakeViewerListDB{}
		if rec := listAgentTemplatesRec(t, db, actor); rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		isAdmin, viewer := viewerQueryArgs(t, db.gotArgs, db.called, 0, 1)
		if isAdmin {
			t.Error("ListAgentTemplatesForViewer received is_admin=true for a NON-ADMIN caller: " +
				"every caller lists as admin and any private template is listable by anyone")
		}
		if !viewer.Valid || uuid.UUID(viewer.Bytes) != actor.ID {
			t.Errorf("viewer_id = %v (valid=%v), want the caller's own id %v",
				uuid.UUID(viewer.Bytes), viewer.Valid, actor.ID)
		}
	})

	t.Run("admin caller", func(t *testing.T) {
		actor := adminCaller()
		db := &fakeViewerListDB{}
		if rec := listAgentTemplatesRec(t, db, actor); rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		isAdmin, viewer := viewerQueryArgs(t, db.gotArgs, db.called, 0, 1)
		if !isAdmin {
			t.Error("ListAgentTemplatesForViewer received is_admin=false for an ADMIN caller: " +
				"admins lose the cross-scope read the flag exists to grant")
		}
		if !viewer.Valid || uuid.UUID(viewer.Bytes) != actor.ID {
			t.Errorf("viewer_id = %v (valid=%v), want the caller's own id %v",
				uuid.UUID(viewer.Bytes), viewer.Valid, actor.ID)
		}
	})
}

// getAgentTemplateRec drives GetAgentTemplate (GET /{id}) for one id as actor.
func getAgentTemplateRec(t *testing.T, db store.DBTX, id uuid.UUID, actor store.User) *httptest.ResponseRecorder {
	t.Helper()
	h := &Handler{q: store.New(db)}
	req := httptest.NewRequest(http.MethodGet, "/api/agent-templates/"+id.String(), nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", id.String())
	req = req.WithContext(context.WithValue(
		mw.ContextWithUser(req.Context(), actor), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()
	h.GetAgentTemplate(rec, req)
	return rec
}

// TestGetAgentTemplatePassesCallerIdentity pins the pass-through into
// GetAgentTemplateForViewer at loadTemplateForViewer (agent_templates.go:624). Mutating
// `IsAdmin: actor.IsAdmin` to `IsAdmin: true` lets any caller fetch any private
// template by id — it survived a full-scope `go test ./...` run. Both flag directions
// are asserted; the admin `== true` check is the backstop no zero value can satisfy.
func TestGetAgentTemplatePassesCallerIdentity(t *testing.T) {
	t.Run("non-admin caller", func(t *testing.T) {
		actor := nonAdminCaller()
		tmpl := store.AgentTemplate{ID: uuid.New(), Name: "t", Scope: "global"}
		db := &fakeTemplateViewerDB{tmpl: &tmpl}
		if rec := getAgentTemplateRec(t, db, tmpl.ID, actor); rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		gotID, isAdmin, viewer := templateViewerArgs(t, db)
		if isAdmin {
			t.Error("GetAgentTemplateForViewer received is_admin=true for a NON-ADMIN caller: " +
				"every caller reads as admin and any private template is readable by anyone")
		}
		if !viewer.Valid || uuid.UUID(viewer.Bytes) != actor.ID {
			t.Errorf("viewer_id = %v (valid=%v), want the caller's own id %v",
				uuid.UUID(viewer.Bytes), viewer.Valid, actor.ID)
		}
		if gotID != tmpl.ID {
			t.Errorf("id = %v, want the path id %v", gotID, tmpl.ID)
		}
	})

	t.Run("admin caller", func(t *testing.T) {
		actor := adminCaller()
		tmpl := store.AgentTemplate{ID: uuid.New(), Name: "t", Scope: "global"}
		db := &fakeTemplateViewerDB{tmpl: &tmpl}
		if rec := getAgentTemplateRec(t, db, tmpl.ID, actor); rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		_, isAdmin, viewer := templateViewerArgs(t, db)
		if !isAdmin {
			t.Error("GetAgentTemplateForViewer received is_admin=false for an ADMIN caller: " +
				"admins lose the cross-scope read the flag exists to grant")
		}
		if !viewer.Valid || uuid.UUID(viewer.Bytes) != actor.ID {
			t.Errorf("viewer_id = %v (valid=%v), want the caller's own id %v",
				uuid.UUID(viewer.Bytes), viewer.Valid, actor.ID)
		}
	})
}
