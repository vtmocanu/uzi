package handler

import (
	"testing"

	"github.com/vtmocanu/uzi/api/internal/agenttmpl"
)

// TestUpdateMarksCustomizedSaveShippedIsPristine is issue #339: saving a
// builtin's shipped content back through the write path must NOT mark the row
// customized, so "Reset to default" then "Save changes" is idempotent with Reset
// and the row keeps tracking future shipped bodies via RefreshPristineBuiltin. It
// mirrors TestNoOpSaveCreatesNoDrift's construction (the shipped DTO submitted
// unchanged through the real validator) for every shipped builtin, asserts the
// no-op save reports customized=false, then mutates one field and asserts a
// genuine edit reports customized=true.
func TestUpdateMarksCustomizedSaveShippedIsPristine(t *testing.T) {
	for _, def := range agenttmpl.Builtins() {
		t.Run(def.Name, func(t *testing.T) {
			row := seededRow(t, def.Name)
			dto := templateDTO(row)
			if dto.DiffersFromBuiltin {
				t.Fatalf("precondition: the seeded row already reports drift")
			}

			// Exactly what AgentTemplateEditor submits when nothing is touched.
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
			if updateMarksCustomized(row, fields) {
				t.Errorf("saving the shipped content must store customized=false")
			}

			// A genuine edit to any one field must mark the row customized.
			edited := req
			edited.PromptBody += "\nAn admin added this line.\n"
			editedFields, err := validateTemplateFields(edited)
			if err != nil {
				t.Fatalf("an edited resubmit was rejected: %v", err)
			}
			if !updateMarksCustomized(row, editedFields) {
				t.Errorf("a genuine edit must store customized=true")
			}
		})
	}
}

// TestUpdateMarksCustomizedNonBuiltin pins that a non-builtin row has no shipped
// definition to track, so an update always marks it customized — even when the
// submitted content is byte-identical to a builtin of the same name. A global
// row colliding with a builtin's shipped body must still store customized=true.
func TestUpdateMarksCustomizedNonBuiltin(t *testing.T) {
	def := agenttmpl.Builtins()[0]
	row := seededRow(t, def.Name)
	row.Scope = "global"
	row.IsBuiltin = false

	dto := templateDTO(row)
	req := templateWriteRequest{
		Description: dto.Description,
		Model:       dto.Model,
		Tools:       dto.Tools,
		PromptBody:  dto.PromptBody,
	}
	fields, err := validateTemplateFields(req)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if !updateMarksCustomized(row, fields) {
		t.Errorf("a non-builtin row must always store customized=true, even for shipped content")
	}
}
