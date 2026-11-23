package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/agenttmpl"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type crossCheckPinDTO = apitypes.CrossCheckPinDTO

// crossCheckPinError is one cross_check_pins defect. Stage, Harness and Member (model or effort)
// identify the offending cell and member when the defect belongs to one; a list-shape defect (too
// many cells, not a list) carries none. Err keeps the original error so the first-error wrapper
// returns the exact text and chain PutMySettings has always reported.
type crossCheckPinError struct {
	Stage, Harness, Member string
	Err                    error
}

// decodeCrossCheckPins returns the decoded pin patches, or the FIRST defect as an error (the
// PutMySettings contract).
func decodeCrossCheckPins(raw json.RawMessage, userID uuid.UUID) ([]store.PatchUserCrossCheckPinParams, error) {
	patches, errs := collectCrossCheckPins(raw, userID)
	if len(errs) > 0 {
		return nil, errs[0].Err
	}
	return patches, nil
}

// collectCrossCheckPins decodes like decodeCrossCheckPins but reports every defect: a list-shape
// defect ends the walk (there are no cells to attribute), otherwise every cell is checked. A cell
// reports only the first of its stage, harness or duplicate defect and is then skipped (its
// members are not validated); a cell that passes those reports one defect per bad model or effort
// member. Defects come in request order. The patches are only meaningful when no error is returned.
func collectCrossCheckPins(raw json.RawMessage, userID uuid.UUID) ([]store.PatchUserCrossCheckPinParams, []crossCheckPinError) {
	if raw == nil {
		return nil, nil
	}
	var cells []struct {
		Stage   string          `json:"stage"`
		Harness string          `json:"harness"`
		Model   json.RawMessage `json:"model"`
		Effort  json.RawMessage `json:"effort"`
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&cells); err != nil || cells == nil || len(cells) > 4 {
		return nil, []crossCheckPinError{{Err: fmt.Errorf("cross_check_pins: expected at most four keyed cells")}}
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return nil, []crossCheckPinError{{Err: fmt.Errorf("cross_check_pins: invalid list")}}
	}
	var errs []crossCheckPinError
	seen := map[string]bool{}
	patches := make([]store.PatchUserCrossCheckPinParams, 0, len(cells))
	for _, c := range cells {
		// Stage and harness are caller text: every echo of them is clipped (clipEcho). A valid
		// stage or harness is short, so the clip changes only the text of oversized unknown ones
		// (and the dedupe key, which is only consulted for valid cells).
		stage, harness := clipEcho(c.Stage), clipEcho(c.Harness)
		key := stage + "/" + harness
		cellErr := func(member string, err error) {
			errs = append(errs, crossCheckPinError{Stage: stage, Harness: harness, Member: member, Err: err})
		}
		if c.Stage != "plan" && c.Stage != "code" {
			cellErr("", fmt.Errorf("cross_check_pins %s stage: unknown stage", key))
			continue
		}
		if c.Harness != "claude" && c.Harness != "codex" {
			cellErr("", fmt.Errorf("cross_check_pins %s harness: unknown harness", key))
			continue
		}
		if seen[key] {
			cellErr("", fmt.Errorf("cross_check_pins %s: duplicate cell", key))
			continue
		}
		seen[key] = true
		patch := store.PatchUserCrossCheckPinParams{UserID: userID, Stage: c.Stage, Harness: c.Harness,
			SetModel: c.Model != nil, SetEffort: c.Effort != nil}
		for _, field := range []struct {
			name     string
			raw      json.RawMessage
			target   *pgtype.Text
			validate func(string, string) (string, error)
		}{{"model", c.Model, &patch.Model, agenttmpl.ValidateFamilyModel},
			{"effort", c.Effort, &patch.Effort, agenttmpl.ValidateFamilyEffort}} {
			if field.raw == nil {
				continue
			}
			var value *string
			if err := json.Unmarshal(field.raw, &value); err != nil {
				cellErr(field.name, fmt.Errorf("cross_check_pins %s %s: expected string or null", key, field.name))
				continue
			}
			if value == nil {
				continue
			}
			normalized, err := field.validate(c.Harness, *value)
			if err != nil {
				cellErr(field.name, fmt.Errorf("cross_check_pins %s %s: %w", key, field.name, err))
				continue
			}
			*field.target = pgtype.Text{String: normalized, Valid: normalized != ""}
		}
		if patch.SetModel || patch.SetEffort {
			patches = append(patches, patch)
		}
	}
	return patches, errs
}

// Each conditional UPSERT preserves omitted columns even when concurrent field
// saves serialize on the cell's unique key. All cells commit or roll back together.
func (h *Handler) saveCrossCheckPins(ctx context.Context, patches []store.PatchUserCrossCheckPinParams) error {
	if len(patches) == 0 {
		return nil
	}
	if h.pool == nil {
		return fmt.Errorf("cross-check pin transaction unavailable")
	}
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := saveCrossCheckPinsTx(ctx, store.New(tx), patches); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// saveCrossCheckPinsTx writes the pin patches through q, which must be bound to the caller's
// transaction. It sorts the patches in place so every writer takes the cell row locks in the
// same order (stage, then harness); a caller that also locks the users row takes that lock first.
func saveCrossCheckPinsTx(ctx context.Context, q *store.Queries, patches []store.PatchUserCrossCheckPinParams) error {
	sort.Slice(patches, func(i, j int) bool {
		if patches[i].Stage != patches[j].Stage {
			return patches[i].Stage < patches[j].Stage
		}
		return patches[i].Harness < patches[j].Harness
	})
	for _, patch := range patches {
		if err := q.PatchUserCrossCheckPin(ctx, patch); err != nil {
			return err
		}
	}
	return nil
}

// crossCheckPinSettings returns the four pin cells and, as second and third values, the lead
// template's Codex pin (template_codex_model) and Claude pin (template_model) from the same rows: the LATERAL
// lead/orchestrator selection is the one rule, identical on every row, so the first
// row carries it (invalid when there are no rows or no pin). The fourth and fifth values are the
// lead template's raw Claude effort (template_effort) and Codex effort (template_codex_effort),
// unfiltered; invalid when unset or there are no rows.
func (h *Handler) crossCheckPinSettings(ctx context.Context, userID uuid.UUID) ([]crossCheckPinDTO, pgtype.Text, pgtype.Text, pgtype.Text, pgtype.Text, error) {
	rows, err := h.q.ListUserCrossCheckPins(ctx, userID)
	if err != nil {
		return nil, pgtype.Text{}, pgtype.Text{}, pgtype.Text{}, pgtype.Text{}, err
	}
	var leadCodexPin, leadClaudePin, leadClaudeEffort, leadCodexEffort pgtype.Text
	if len(rows) > 0 {
		leadCodexPin = rows[0].TemplateCodexModel
		leadClaudePin = rows[0].TemplateModel
		leadClaudeEffort = rows[0].TemplateEffort
		leadCodexEffort = rows[0].TemplateCodexEffort
	}
	cells := make([]crossCheckPinDTO, 0, len(rows))
	for _, row := range rows {
		model := textPtrValue(row.Model.Valid, row.Model.String)
		effort := textPtrValue(row.Effort.Valid, row.Effort.String)
		workerModel := row.DefaultClaudeModel
		if row.Harness == "codex" {
			workerModel = row.DefaultCodexModel
		}
		resolved := agenttmpl.ResolveCrossCheck(row.Harness, model, effort,
			textPtrValue(workerModel.Valid, workerModel.String),
			textPtrValue(row.TemplateModel.Valid, row.TemplateModel.String),
			textPtrValue(row.TemplateCodexModel.Valid, row.TemplateCodexModel.String))
		workerDefault := agenttmpl.ResolveCrossCheck(row.Harness, nil, nil,
			textPtrValue(workerModel.Valid, workerModel.String),
			textPtrValue(row.TemplateModel.Valid, row.TemplateModel.String),
			textPtrValue(row.TemplateCodexModel.Valid, row.TemplateCodexModel.String))
		cells = append(cells, crossCheckPinDTO{Stage: row.Stage, Harness: row.Harness, WorkerDefaultModel: workerDefault.Model,
			Model: model, Effort: effort, ResolvedModel: resolved.Model, ResolvedEffort: resolved.Effort,
			ModelSource: resolved.ModelSource, EffortSource: resolved.EffortSource, Active: true})
	}
	return cells, leadCodexPin, leadClaudePin, leadClaudeEffort, leadCodexEffort, nil
}
