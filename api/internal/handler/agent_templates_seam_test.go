package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/agenttmpl"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// fakeTemplateWriteStore satisfies agentTemplateWriteStore so the builtin-
// definition and reset handlers can be exercised without a database (issue #223).
// GetAgentTemplate returns a canned row; ResetBuiltinAgentTemplate records the
// params it was called with so a test can pin what content the reset re-applies.
type fakeTemplateWriteStore struct {
	getRow      store.AgentTemplate
	getErr      error
	resetRow    store.AgentTemplate
	resetErr    error
	resetCalled bool
	resetArg    store.ResetBuiltinAgentTemplateParams
}

func (f *fakeTemplateWriteStore) GetAgentTemplate(context.Context, uuid.UUID) (store.AgentTemplate, error) {
	return f.getRow, f.getErr
}

func (f *fakeTemplateWriteStore) ResetBuiltinAgentTemplate(_ context.Context, arg store.ResetBuiltinAgentTemplateParams) (store.AgentTemplate, error) {
	f.resetCalled = true
	f.resetArg = arg
	return f.resetRow, f.resetErr
}

// seamRequest builds the request the seam handlers expect: the {id} route param
// set to the fake's row id, and the actor in context. Mirrors getAgentTemplateRec.
func seamRequest(actor store.User, id uuid.UUID) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/agent-templates/"+id.String()+"/builtin", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", id.String())
	return req.WithContext(context.WithValue(
		mw.ContextWithUser(req.Context(), actor), chi.RouteCtxKey, rctx))
}

// TestGetBuiltinAgentTemplateServesShippedNotStored kills the mutation that serves
// templateDTO(t)/the stored row from GetBuiltinAgentTemplate instead of the shipped
// definition: the stored row is edited so its content differs, and the 200 must
// still carry the SHIPPED body.
func TestGetBuiltinAgentTemplateServesShippedNotStored(t *testing.T) {
	def := builtinWithTools(t)
	name := def.Name
	if _, ok := agenttmpl.BuiltinByName(name); !ok {
		t.Fatalf("builtin %q is not shipped by this binary", name)
	}

	getRow := seededRow(t, name)
	getRow.PromptBody += "\n\nADMIN EDIT"
	getRow.Description = getRow.Description + " (edited)"

	// Positive control: the stored row must genuinely differ from the shipped
	// definition, else serving either would pass and the test would be vacuous.
	if !differsFromBuiltin(getRow) {
		t.Fatal("precondition: the mutated stored row must differ from the shipped builtin, else this test is vacuous")
	}

	fake := &fakeTemplateWriteStore{getRow: getRow}
	h := &Handler{tmplWriteStore: fake}

	rec := httptest.NewRecorder()
	h.GetBuiltinAgentTemplate(rec, seamRequest(store.User{ID: uuid.New(), IsAdmin: true}, getRow.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body)
	}

	var body struct {
		Builtin builtinDefinitionDTO `json:"builtin"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	want := builtinDefinitionView(def)
	if body.Builtin.PromptBody != want.PromptBody {
		t.Errorf("prompt_body = %q, want the shipped %q", body.Builtin.PromptBody, want.PromptBody)
	}
	// The mutation this kills: serving templateDTO(t)/the stored row instead of the
	// shipped definition would return the edited prompt body.
	if body.Builtin.PromptBody == getRow.PromptBody {
		t.Error("prompt_body equals the stored row's edited body: serving templateDTO(t)/the stored row instead of the shipped definition")
	}
	if body.Builtin.Description != want.Description {
		t.Errorf("description = %q, want the shipped %q", body.Builtin.Description, want.Description)
	}
	if body.Builtin.Description == getRow.Description {
		t.Error("description equals the stored row's edited value: serving the stored row instead of the shipped definition")
	}
	if !reflect.DeepEqual(body.Builtin.Model, want.Model) {
		t.Errorf("model = %v, want the shipped %v", body.Builtin.Model, want.Model)
	}
	if !reflect.DeepEqual(body.Builtin.Tools, want.Tools) {
		t.Errorf("tools = %v, want the shipped %v", body.Builtin.Tools, want.Tools)
	}
}

// TestResetAgentTemplateClearsBadgeServerSide kills the mutation that returns the
// pre-reset templateDTO(t) (the drifted stored row) from ResetAgentTemplate so the
// badge never clears: the response must carry differs_from_builtin=false and the
// shipped content, and the reset query must be called with the shipped columns.
func TestResetAgentTemplateClearsBadgeServerSide(t *testing.T) {
	def := builtinWithTools(t)
	name := def.Name
	if _, ok := agenttmpl.BuiltinByName(name); !ok {
		t.Fatalf("builtin %q is not shipped by this binary", name)
	}

	// A DRIFTED builtin row: what loadTemplateForWrite fetches before the reset.
	getRow := seededRow(t, name)
	getRow.PromptBody += "\n\nADMIN EDIT"
	if !differsFromBuiltin(getRow) {
		t.Fatal("precondition: the pre-reset row must report drift, else the badge-clear assertion is vacuous")
	}

	// The pristine row the reset query returns.
	resetRow := seededRow(t, name)
	if differsFromBuiltin(resetRow) {
		t.Fatal("precondition: the post-reset row must be pristine (no drift)")
	}

	fake := &fakeTemplateWriteStore{getRow: getRow, resetRow: resetRow}
	h := &Handler{tmplWriteStore: fake}

	rec := httptest.NewRecorder()
	h.ResetAgentTemplate(rec, seamRequest(adminCaller(), getRow.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body)
	}

	var body struct {
		Template agentTemplateDTO `json:"template"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	// The badge-cleared contract. The mutation this kills: returning the pre-reset
	// templateDTO(t) so the badge never clears.
	if body.Template.DiffersFromBuiltin {
		t.Error("differs_from_builtin = true after reset: returning the pre-reset templateDTO(t) so the badge never clears")
	}
	if body.Template.PromptBody != def.PromptBody {
		t.Errorf("prompt_body = %q, want the shipped %q", body.Template.PromptBody, def.PromptBody)
	}
	if body.Template.Description != def.Description {
		t.Errorf("description = %q, want the shipped %q", body.Template.Description, def.Description)
	}

	// The reset query must have been called, and with the shipped content — pinning
	// that reset re-applies the shipped definition rather than the stored row.
	if !fake.resetCalled {
		t.Fatal("ResetBuiltinAgentTemplate was never called")
	}
	if fake.resetArg.Description != def.Description {
		t.Errorf("reset arg description = %q, want the shipped %q", fake.resetArg.Description, def.Description)
	}
	if fake.resetArg.PromptBody != def.PromptBody {
		t.Errorf("reset arg prompt_body = %q, want the shipped %q", fake.resetArg.PromptBody, def.PromptBody)
	}
	wantModel, wantTools, err := storeColumns(def)
	if err != nil {
		t.Fatalf("encode shipped columns: %v", err)
	}
	if !reflect.DeepEqual(fake.resetArg.Model, wantModel) {
		t.Errorf("reset arg model = %v, want the shipped %v", fake.resetArg.Model, wantModel)
	}
	if !reflect.DeepEqual(fake.resetArg.Tools, wantTools) {
		t.Errorf("reset arg tools = %s, want the shipped %s", fake.resetArg.Tools, wantTools)
	}
}
