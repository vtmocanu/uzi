package handler

import (
	"encoding/json"
	"fmt"
	"strings"

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

// validateTemplateEffort validates a template's per-harness effort pin. A nil, blank or
// whitespace-only value is NULL (no pin), like agenttmpl.ValidateEffort; `runtime` and any
// value outside agenttmpl.EffortLevels is an error (mapped to 400 by the caller).
func validateTemplateEffort(field string, raw *string) (pgtype.Text, error) {
	value := ""
	if raw != nil {
		value = *raw
	}
	e, err := agenttmpl.ValidateEffort(value)
	if err != nil {
		return pgtype.Text{}, fmt.Errorf("%s: %w", field, err)
	}
	return pgtype.Text{String: e, Valid: e != ""}, nil
}

// shippedEffort maps a builtin definition's effort onto the column type, blank as NULL.
func shippedEffort(v string) pgtype.Text {
	v = strings.TrimSpace(v)
	return pgtype.Text{String: v, Valid: v != ""}
}

// mergeOmittedTemplatePins runs against the locked row before comparison/write.
func mergeOmittedTemplatePins(req templateWriteRequest, fields validatedFields, current store.AgentTemplate) validatedFields {
	if !req.CodexModel.Present {
		fields.codexModel = current.CodexModel
	}
	if !req.Effort.Present {
		fields.effort = current.Effort
	}
	if !req.CodexEffort.Present {
		fields.codexEffort = current.CodexEffort
	}
	return fields
}
