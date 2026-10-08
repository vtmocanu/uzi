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

func decodeCrossCheckPins(raw json.RawMessage, userID uuid.UUID) ([]store.PatchUserCrossCheckPinParams, error) {
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
	if err := d.Decode(&cells); err != nil || cells == nil || len(cells) > 2 {
		return nil, fmt.Errorf("cross_check_pins: expected at most two keyed cells")
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("cross_check_pins: invalid list")
	}
	seen := map[string]bool{}
	patches := make([]store.PatchUserCrossCheckPinParams, 0, len(cells))
	for _, c := range cells {
		key := c.Stage + "/" + c.Harness
		if c.Stage != "plan" {
			return nil, fmt.Errorf("cross_check_pins %s stage: unknown stage", key)
		}
		if c.Harness != "claude" && c.Harness != "codex" {
			return nil, fmt.Errorf("cross_check_pins %s harness: unknown harness", key)
		}
		if seen[key] {
			return nil, fmt.Errorf("cross_check_pins %s: duplicate cell", key)
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
				return nil, fmt.Errorf("cross_check_pins %s %s: expected string or null", key, field.name)
			}
			if value == nil {
				continue
			}
			normalized, err := field.validate(c.Harness, *value)
			if err != nil {
				return nil, fmt.Errorf("cross_check_pins %s %s: %w", key, field.name, err)
			}
			*field.target = pgtype.Text{String: normalized, Valid: normalized != ""}
		}
		if patch.SetModel || patch.SetEffort {
			patches = append(patches, patch)
		}
	}
	return patches, nil
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
	q := store.New(tx)
	// Acquire cell locks in the same order for every saveCrossCheckPins request.
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
	return tx.Commit(ctx)
}

func (h *Handler) crossCheckPinSettings(ctx context.Context, userID uuid.UUID) ([]crossCheckPinDTO, error) {
	rows, err := h.q.ListUserCrossCheckPins(ctx, userID)
	if err != nil {
		return nil, err
	}
	cells := make([]crossCheckPinDTO, 0, len(rows))
	for _, row := range rows {
		model := textPtrValue(row.Model.Valid, row.Model.String)
		effort := textPtrValue(row.Effort.Valid, row.Effort.String)
		workerModel, workerEffort := row.DefaultClaudeModel, row.DefaultEffort
		if row.Harness == "codex" {
			workerModel, workerEffort = row.DefaultCodexModel, row.DefaultCodexEffort
		}
		resolved := agenttmpl.ResolveCrossCheck(row.Harness, model, effort,
			textPtrValue(workerModel.Valid, workerModel.String),
			textPtrValue(workerEffort.Valid, workerEffort.String),
			textPtrValue(row.TemplateModel.Valid, row.TemplateModel.String))
		workerDefault := agenttmpl.ResolveCrossCheck(row.Harness, nil, nil,
			textPtrValue(workerModel.Valid, workerModel.String),
			textPtrValue(workerEffort.Valid, workerEffort.String),
			textPtrValue(row.TemplateModel.Valid, row.TemplateModel.String))
		cells = append(cells, crossCheckPinDTO{Stage: row.Stage, Harness: row.Harness, WorkerDefaultModel: workerDefault.Model,
			Model: model, Effort: effort, ResolvedModel: resolved.Model, ResolvedEffort: resolved.Effort,
			ModelSource: resolved.ModelSource, EffortSource: resolved.EffortSource, Active: row.Harness == "codex"})
	}
	return cells, nil
}
