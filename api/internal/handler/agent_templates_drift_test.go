package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/agenttmpl"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// seededRow builds the row a boot-time reconcile would produce for a builtin:
// the shipped definition through the very column encoder both the seeder and
// Reset use (storeColumns mirrors store.builtinColumns). Every drift case below
// starts here, so a case that reports drift is reporting on the EDIT it applied
// and not on an artefact of how the fixture was constructed.
func seededRow(t *testing.T, name string) store.AgentTemplate {
	t.Helper()
	def, ok := agenttmpl.BuiltinByName(name)
	if !ok {
		t.Fatalf("builtin %q is not shipped by this binary", name)
	}
	model, tools, err := storeColumns(def)
	if err != nil {
		t.Fatalf("encode builtin %q: %v", name, err)
	}
	return store.AgentTemplate{
		ID:          uuid.New(),
		Name:        def.Name,
		Description: def.Description,
		Model:       model,
		Tools:       tools,
		PromptBody:  def.PromptBody,
		IsBuiltin:   true,
		Scope:       "builtin",
	}
}

// builtinWithTools picks a shipped builtin that actually carries a tools
// allowlist. Two of the eleven (coder, lead) carry none, so a tools case built
// on the wrong one would compare empty against empty and pass whatever the
// implementation does.
func builtinWithTools(t *testing.T) agenttmpl.Definition {
	t.Helper()
	for _, def := range agenttmpl.Builtins() {
		if len(def.Tools) > 1 {
			return def
		}
	}
	t.Fatal("no shipped builtin carries a multi-entry tools list; the tools cases below would be vacuous")
	return agenttmpl.Definition{}
}

// TestDiffersFromBuiltinPerColumn is the four-column matrix from the milestone's
// criterion 2: a pristine row reports false, and an edit to ANY ONE of the four
// mutable columns reports true. An implementation comparing only prompt_body
// goes red on three of these.
func TestDiffersFromBuiltinPerColumn(t *testing.T) {
	withTools := builtinWithTools(t)

	if differsFromBuiltin(seededRow(t, withTools.Name)) {
		t.Error("a pristine seeded builtin must not report drift")
	}

	// Non-vacuity guard for the order-only case below, which swaps the first two
	// tools: were they ever equal, the swap would be a no-op and the case would
	// pass against an implementation that sorts — silently, which is the failure
	// this whole file exists to make impossible. Its sibling
	// TestDiffersFromBuiltinPostgresCanonicalTools carries the same shape.
	if len(withTools.Tools) < 2 || withTools.Tools[0] == withTools.Tools[1] {
		t.Fatalf("precondition: %q ships tools %v — the order case needs two DISTINCT leading entries",
			withTools.Name, withTools.Tools)
	}

	cases := []struct {
		column string
		edit   func(*store.AgentTemplate)
	}{
		{"description", func(r *store.AgentTemplate) { r.Description += " Edited." }},
		{"model", func(r *store.AgentTemplate) {
			// Flip between set and NULL either way, so the case is real whichever
			// the chosen builtin ships.
			if r.Model.Valid {
				r.Model.Valid = false
				r.Model.String = ""
			} else {
				r.Model.String = "haiku"
				r.Model.Valid = true
			}
		}},
		{"tools (membership)", func(r *store.AgentTemplate) {
			r.Tools = mustJSON(t, withTools.Tools[:len(withTools.Tools)-1])
		}},
		{"tools (order only)", func(r *store.AgentTemplate) {
			swapped := append([]string(nil), withTools.Tools...)
			swapped[0], swapped[1] = swapped[1], swapped[0]
			r.Tools = mustJSON(t, swapped)
		}},
		{"prompt_body", func(r *store.AgentTemplate) { r.PromptBody += "\nAn admin added this line.\n" }},
	}
	for _, c := range cases {
		t.Run(c.column, func(t *testing.T) {
			row := seededRow(t, withTools.Name)
			c.edit(&row)
			if !differsFromBuiltin(row) {
				t.Errorf("an edit to %s must report drift", c.column)
			}
		})
	}
}

// TestDiffersFromBuiltinWhitespaceOnlyEdit pins the never-trim rule from the
// comparison side: an edit visible only as trailing whitespace is still an edit
// to the file the worker writes, and a trimming comparison would hide it forever.
func TestDiffersFromBuiltinWhitespaceOnlyEdit(t *testing.T) {
	row := seededRow(t, "coder")
	row.PromptBody += "\n"
	if !differsFromBuiltin(row) {
		t.Error("a trailing-newline edit to the prompt body must report drift")
	}
}

// TestDiffersFromBuiltinIgnoresNonBuiltinScopes is criterion 3, and the user case
// is the one that separates a scope-checking implementation from a name-only one.
// 00048 explicitly allows a user to own a 'coder' alongside the builtin 'coder';
// badging that private row would advertise a Reset that answers 400.
func TestDiffersFromBuiltinIgnoresNonBuiltinScopes(t *testing.T) {
	def, ok := agenttmpl.BuiltinByName("coder")
	if !ok {
		t.Fatal("builtin coder missing")
	}

	// Deliberately DIFFERENT content under a builtin's name: a name-only
	// implementation reports drift here, a scope-checking one reports false.
	collide := func(scope string) store.AgentTemplate {
		row := store.AgentTemplate{
			ID:          uuid.New(),
			Name:        def.Name,
			Description: "My own take on a coder.",
			PromptBody:  "You are my personal coder.\n",
			IsBuiltin:   false,
			Scope:       scope,
		}
		if scope == "user" {
			row.UserID = pgUUID(uuid.New())
		}
		return row
	}

	for _, scope := range []string{"user", "global"} {
		t.Run(scope+" scope colliding with a builtin name", func(t *testing.T) {
			if differsFromBuiltin(collide(scope)) {
				t.Errorf("a %s-scope row has no shipped counterpart and must report false", scope)
			}
		})
	}
}

// TestDiffersFromBuiltinRemovedBuiltinReportsFalse is criterion 4: a builtin row
// this release no longer ships has nothing to compare against. It reports false
// even though Reset would answer 409 — the two states are told apart by the
// /builtin route, not by this field.
func TestDiffersFromBuiltinRemovedBuiltinReportsFalse(t *testing.T) {
	if _, ok := agenttmpl.BuiltinByName("retired-role"); ok {
		t.Fatal("fixture assumes 'retired-role' is not a shipped builtin")
	}
	row := store.AgentTemplate{
		ID:          uuid.New(),
		Name:        "retired-role",
		Description: "A builtin a later release dropped.",
		PromptBody:  "You are retired.\n",
		IsBuiltin:   true,
		Scope:       "builtin",
	}
	if differsFromBuiltin(row) {
		t.Error("a builtin with no shipped definition must report false, not true")
	}
}

// TestDiffersFromBuiltinPostgresCanonicalTools is the jsonb hazard, pinned
// WITHOUT a database. Postgres re-serializes a jsonb array with a space after
// each comma, so the bytes pgx reads back are not the bytes json.Marshal wrote —
// a raw byte comparison would report drift on a pristine row. This hands the
// comparison exactly that form.
func TestDiffersFromBuiltinPostgresCanonicalTools(t *testing.T) {
	def := builtinWithTools(t)
	row := seededRow(t, def.Name)

	canonical := postgresJSONArray(def.Tools)
	if string(row.Tools) == canonical {
		t.Fatalf("precondition: the seeded bytes %q already match the Postgres form, so this case is vacuous", row.Tools)
	}
	row.Tools = []byte(canonical)

	if differsFromBuiltin(row) {
		t.Errorf("the Postgres-canonical tools encoding %q must not read as drift", canonical)
	}
}

// TestNoOpSaveCreatesNoDrift is criterion 8, and it is the single test that
// catches a seed-path/write-path asymmetry in any of the four columns: seed a
// pristine builtin the way boot does, push it through the DTO the editor loads,
// submit it back unchanged through the very validator the write path runs, and
// assert the resulting row still reports no drift. Nothing else reaches it —
// every other case here constructs both sides through the same encoder.
func TestNoOpSaveCreatesNoDrift(t *testing.T) {
	for _, def := range agenttmpl.Builtins() {
		t.Run(def.Name, func(t *testing.T) {
			row := seededRow(t, def.Name)
			dto := templateDTO(row)
			if dto.DiffersFromBuiltin {
				t.Fatalf("precondition: the seeded row already reports drift")
			}

			// Exactly what AgentTemplateEditor submits when nothing is touched:
			// the loaded values, with a blank model sent as null and an empty
			// tools list sent as null.
			req := templateWriteRequest{
				Description: dto.Description,
				Model:       dto.Model,
				Tools:       dto.Tools,
				PromptBody:  dto.PromptBody,
			}
			fields, err := validateTemplateFields(req)
			if err != nil {
				t.Fatalf("a no-op resubmit of a shipped builtin was rejected: %v", err)
			}

			// The row UpdateAgentTemplate would write back.
			saved := row
			saved.Description = fields.description
			saved.Model = fields.model
			saved.Tools = fields.tools
			saved.PromptBody = fields.promptBody

			if differsFromBuiltin(saved) {
				t.Errorf("a no-op save created drift: description=%q model=%v tools=%s",
					saved.Description, saved.Model, saved.Tools)
			}
		})
	}
}

// TestTemplateDTOCarriesDriftField pins the wire name and the type. The web type
// declares it non-optional, so a rename here breaks the client loudly rather than
// leaving the badge silently absent.
func TestTemplateDTOCarriesDriftField(t *testing.T) {
	row := seededRow(t, "coder")
	row.PromptBody += "edited\n"

	b, err := json.Marshal(templateDTO(row))
	if err != nil {
		t.Fatalf("marshal dto: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal dto: %v", err)
	}
	v, present := got["differs_from_builtin"]
	if !present {
		t.Fatalf("dto is missing differs_from_builtin: %s", b)
	}
	if v != true {
		t.Errorf("differs_from_builtin = %v, want true for an edited row", v)
	}
}

// TestWriteBuiltinDefinitionStatusMatrix is criterion 5, exercised over a real
// ResponseRecorder so the statuses and the body are the ones a client sees. Only
// the row fetch is out of frame (it needs a database); everything the endpoint
// decides is here.
func TestWriteBuiltinDefinitionStatusMatrix(t *testing.T) {
	admin := store.User{ID: uuid.New(), IsAdmin: true}
	member := store.User{ID: uuid.New()}
	owner := store.User{ID: uuid.New()}

	builtin := seededRow(t, "coder")

	removed := store.AgentTemplate{
		ID: uuid.New(), Name: "retired-role", Description: "gone.",
		PromptBody: "x\n", IsBuiltin: true, Scope: "builtin",
	}
	global := store.AgentTemplate{
		ID: uuid.New(), Name: "release-notes", Description: "drafts notes.",
		PromptBody: "x\n", Scope: "global",
	}
	private := store.AgentTemplate{
		ID: uuid.New(), Name: "mira-helper", Description: "helps mira.",
		PromptBody: "x\n", Scope: "user", UserID: pgUUID(owner.ID),
	}

	cases := []struct {
		name       string
		actor      store.User
		row        store.AgentTemplate
		wantStatus int
	}{
		{"admin reads a shipped builtin", admin, builtin, http.StatusOK},
		{"non-admin is refused a builtin", member, builtin, http.StatusForbidden},
		{"a non-owner never learns a private template exists", member, private, http.StatusNotFound},
		{"the owner of a user template gets the no-shipped-definition rule", owner, private, http.StatusBadRequest},
		{"a global template has no shipped definition", admin, global, http.StatusBadRequest},
		{"a removed builtin conflicts", admin, removed, http.StatusConflict},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeBuiltinDefinition(rec, c.actor, c.row)
			if rec.Code != c.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, c.wantStatus, rec.Body)
			}
		})
	}
}

// TestWriteBuiltinDefinitionServesTheShippedBody pins what the 200 carries: the
// shipped definition, not the stored row. The row is deliberately edited first,
// so a handler serving the row instead of the builtin fails here.
func TestWriteBuiltinDefinitionServesTheShippedBody(t *testing.T) {
	def := builtinWithTools(t)
	row := seededRow(t, def.Name)
	row.Description = "AN ADMIN EDITED THIS."
	row.PromptBody = "AND THIS.\n"
	row.Tools = mustJSON(t, []string{"Bash"})

	rec := httptest.NewRecorder()
	writeBuiltinDefinition(rec, store.User{ID: uuid.New(), IsAdmin: true}, row)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body)
	}

	var body struct {
		Builtin builtinDefinitionDTO `json:"builtin"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Builtin.Description != def.Description {
		t.Errorf("description = %q, want the shipped %q", body.Builtin.Description, def.Description)
	}
	if body.Builtin.PromptBody != def.PromptBody {
		t.Error("prompt body is not the shipped one")
	}
	if len(body.Builtin.Tools) != len(def.Tools) {
		t.Errorf("tools = %v, want the shipped %v", body.Builtin.Tools, def.Tools)
	}
	// model null means inherit, matching agentTemplateDTO's convention.
	if def.Model == "" && body.Builtin.Model != nil {
		t.Errorf("model = %v, want null for a builtin that inherits", *body.Builtin.Model)
	}
	if def.Model != "" && (body.Builtin.Model == nil || *body.Builtin.Model != def.Model) {
		t.Errorf("model = %v, want %q", body.Builtin.Model, def.Model)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// postgresJSONArray writes a string slice the way Postgres re-serializes a jsonb
// array: a space after each comma. Built by hand rather than with json.Marshal
// precisely because json.Marshal is the encoding this is meant to differ from.
func postgresJSONArray(items []string) string {
	out := "["
	for i, s := range items {
		if i > 0 {
			out += ", "
		}
		out += `"` + s + `"`
	}
	return out + "]"
}
