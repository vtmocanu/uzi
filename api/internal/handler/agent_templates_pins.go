package handler

import (
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/agenttmpl"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// optionalTemplatePin distinguishes omission from an explicit clear.
type optionalTemplatePin struct {
	Present bool
	Value   *string
}

func (o *optionalTemplatePin) UnmarshalJSON(b []byte) error {
	o.Present = true
	if string(b) == "null" {
		o.Value = nil
		return nil
	}
	var value string
	if err := json.Unmarshal(b, &value); err != nil {
		return err
	}
	o.Value = &value
	return nil
}

func validateTemplatePin(harness, field string, raw *string) (pgtype.Text, error) {
	value := ""
	if raw != nil {
		value = *raw
	}
	model, err := agenttmpl.ValidateTemplatePin(harness, value)
	if err != nil {
		return pgtype.Text{}, fmt.Errorf("%s: %w", field, err)
	}
	return pgtype.Text{String: model, Valid: model != ""}, nil
}

// mergeOmittedTemplatePins runs against the locked row before comparison/write.
func mergeOmittedTemplatePins(req templateWriteRequest, fields validatedFields, current store.AgentTemplate) validatedFields {
	if !req.CodexModel.Present {
		fields.codexModel = current.CodexModel
	}
	return fields
}
