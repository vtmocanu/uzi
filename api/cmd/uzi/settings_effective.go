package main

import (
	"fmt"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// settingsGetJSON is the `uzi settings get --json` body: every key the settings read carries
// today, plus the effective-settings view under "effective" (null when the server does not have
// it or the read failed).
type settingsGetJSON struct {
	apitypes.UserSettingsDTO
	Effective *apitypes.EffectiveSettingsDTO `json:"effective"`
}

// effectiveModelLabels and effectiveEffortLabels name the effective-settings cells the "Model settings" table shows
// under their own setting names. Judge, summary and cross-check settings are printed from the
// settings read, so each setting appears once.
var effectiveModelLabels = map[string]string{
	"worker_model":      "worker model",
	"task_review_model": "task review model",
}

var effectiveEffortLabels = map[string]string{
	"worker_effort":      "worker effort",
	"task_review_effort": "task review effort",
}

// sdkDefaultText stands in for an empty model: the Claude SDK or account decides.
const sdkDefaultText = "SDK/account default"

func orSDKDefault(model string) string {
	if model == "" {
		return sdkDefaultText
	}
	return model
}

// effectiveSettingsRows is the "Model settings" table when the server has the effective view:
// the three start choices, the worker and task-review model and effort cells per harness, and the
// judge and summary rows the settings read already carries. At most 12 model cells, 8 effort cells
// and 3 start choices are read, so a hostile response cannot inflate the table.
func effectiveSettingsRows(s apitypes.UserSettingsDTO, e *apitypes.EffectiveSettingsDTO) []settingRow {
	modelRows := func(setting string) []settingRow {
		var rows []settingRow
		for _, c := range e.Models[:min(len(e.Models), 12)] {
			if c.Setting != setting {
				continue
			}
			rows = append(rows, settingRow{
				Setting: effectiveModelLabels[setting], Harness: c.Harness, Stored: c.Model,
				Default: orSDKDefault(c.DefaultModel), Resolved: orSDKDefault(c.ResolvedModel),
				Field: "model", Source: c.Source, Notes: c.Notes,
			})
		}
		return rows
	}
	effortRows := func(setting string) []settingRow {
		var rows []settingRow
		for _, c := range e.Efforts[:min(len(e.Efforts), 8)] {
			if c.Setting != setting {
				continue
			}
			row := effortRow(effectiveEffortLabels[setting], c.EffortSettingCellDTO)
			row.Notes = c.Notes
			rows = append(rows, row)
		}
		return rows
	}
	rows := effectiveStartRows(e)
	rows = append(rows, modelRows("worker_model")...)
	rows = append(rows, effortRows("worker_effort")...)
	rows = append(rows, modelSettingRows("judge", s.JudgeModels)...)
	rows = append(rows, modelSettingRows("summary", s.SummaryModels)...)
	rows = append(rows, modelRows("task_review_model")...)
	return append(rows, effortRows("task_review_effort")...)
}

// effectiveStartRows renders the start choices: what a start with that harness choice resolves to
// (or the refusal), with the harness-source label as SOURCE.
func effectiveStartRows(e *apitypes.EffectiveSettingsDTO) []settingRow {
	var rows []settingRow
	for _, c := range e.Start[:min(len(e.Start), 3)] {
		row := settingRow{Setting: "start (" + c.Choice + ")", Harness: "-", StoredText: "-", Default: "-"}
		switch {
		case c.Harness == nil:
			row.Resolved = "refused: " + refusalText(c.Refusal, c.RefusalDetail)
		case c.Harness.Value == nil:
			row.Resolved = "claude or codex"
			row.Field, row.Source, row.Notes = "harness", c.Harness.Source, c.Harness.Notes
		default:
			row.Resolved = *c.Harness.Value
			row.Field, row.Source, row.Notes = "harness", c.Harness.Source, c.Harness.Notes
		}
		rows = append(rows, row)
	}
	return rows
}

func refusalText(refusal, detail *string) string {
	if refusal == nil {
		return "unknown"
	}
	if detail != nil {
		return *refusal + " (" + *detail + ")"
	}
	return *refusal
}

// effectiveAlternativeLines explains a start choice whose harness is chosen when the run is
// created: each harness it can land on, with its credential or, when refused, why.
func effectiveAlternativeLines(e *apitypes.EffectiveSettingsDTO) []string {
	var lines []string
	for _, c := range e.Start[:min(len(e.Start), 3)] {
		if c.Harness == nil || c.Harness.Value != nil {
			continue
		}
		lines = append(lines, crossCheckPlain("start ("+c.Choice+") is chosen when the run is created; it can run on:"))
		for _, h := range e.Harnesses[:min(len(e.Harnesses), 2)] {
			text := "refused: " + refusalText(h.Refusal, h.RefusalDetail)
			if h.Usable && h.Credential != nil {
				text = modelSettingSource("credential", h.Credential.Source)
			}
			lines = append(lines, "  "+crossCheckPlain(fmt.Sprintf("%s: %s", h.Harness, text)))
		}
	}
	return lines
}
