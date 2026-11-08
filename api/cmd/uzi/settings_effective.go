package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// settingsGetJSON is the `uzi settings get --json` body: every key the settings read carries
// today, plus the effective-settings view under "effective" (null when the server does not have
// it or the read failed).
type settingsGetJSON struct {
	apitypes.UserSettingsDTO
	Effective *apitypes.EffectiveSettingsDTO `json:"effective"`
	// SchedulePreview is present only with --schedule.
	SchedulePreview *apitypes.ScheduleSettingsPreviewDTO `json:"schedule_preview,omitempty"`
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

// readSchedulePreview reads the schedule preview for `uzi settings get --schedule`. The caller named
// the schedule, so any answer that is not a server or network failure ends the command with its exit
// code: a 404 means the schedule does not exist for this account (or the server predates the
// route). A 5xx or network failure leaves the preview nil with a note on stderr, as the effective
// view degrades, because the settings read itself succeeded.
func readSchedulePreview(ctx context.Context, c uzicli.Client, id string, stderr io.Writer) (*apitypes.ScheduleSettingsPreviewDTO, error) {
	dto, err := c.PreviewScheduleSettings(ctx, id)
	if err == nil {
		return &dto, nil
	}
	var ee *uzicli.ExitError
	if errors.As(err, &ee) && ee.Code != uzicli.ExitUnreachable {
		return nil, err
	}
	_, _ = fmt.Fprintf(stderr, "Schedule preview: unavailable (%s)\n", crossCheckPlain(err.Error()))
	return nil, nil
}

// schedulePreviewRows renders the schedule preview in the model-settings table: the harness (or the
// refusal), then the worker model, worker effort, credential and subagent rows, once for an exact
// harness and once per harness when the harness is chosen when the run is created. At most two
// alternatives are read.
func schedulePreviewRows(d *apitypes.ScheduleSettingsPreviewDTO) []settingRow {
	row := settingRow{Setting: "schedule harness", Harness: "-", StoredText: "-", Default: "-", Notes: d.Notes}
	switch {
	case d.Harness == nil:
		row.Resolved = "refused: " + refusalText(d.Refusal, d.RefusalDetail)
		row.Notes = nil
	case d.Harness.Value == nil:
		row.Resolved = "claude or codex"
		row.Field, row.Source = "harness", d.Harness.Source
		row.Notes = append(append([]string{}, d.Harness.Notes...), d.Notes...)
		if d.Refusal != nil {
			row.Resolved = "refused: " + refusalText(d.Refusal, d.RefusalDetail)
		}
	default:
		row.Resolved = *d.Harness.Value
		row.Field, row.Source = "harness", d.Harness.Source
		row.Notes = append(append([]string{}, d.Harness.Notes...), d.Notes...)
		if d.Refusal != nil {
			row.Resolved += ", refused: " + refusalText(d.Refusal, d.RefusalDetail)
		}
	}
	rows := []settingRow{row}
	if d.Harness != nil && d.Harness.Value != nil && d.Refusal == nil {
		rows = append(rows, scheduleBranchRows(*d.Harness.Value, d.WorkerModel, d.WorkerEffort, d.Credential, d.Subagents, nil, nil)...)
	}
	for _, a := range d.Alternatives[:min(len(d.Alternatives), 2)] {
		rows = append(rows, scheduleBranchRows(a.Harness, a.WorkerModel, a.WorkerEffort, a.Credential, a.Subagents, a.Refusal, a.RefusalDetail)...)
	}
	return rows
}

// scheduleBranchRows are the rows of one harness: its fire refusal, or the worker model, worker
// effort, credential and subagent rows.
func scheduleBranchRows(harness string, model *apitypes.EffectiveModelCellDTO, effort *apitypes.EffectiveEffortCellDTO,
	cred *apitypes.CredentialDTO, subs *apitypes.EffectiveSubagentsDTO, refusal, detail *string) []settingRow {
	if refusal != nil {
		return []settingRow{{Setting: "fire", Harness: harness, StoredText: "-", Default: "-", Resolved: "refused: " + refusalText(refusal, detail)}}
	}
	var rows []settingRow
	if model != nil {
		rows = append(rows, settingRow{
			Setting: "worker model", Harness: harness, Stored: model.Model,
			Default: orSDKDefault(model.DefaultModel), Resolved: orSDKDefault(model.ResolvedModel),
			Field: "model", Source: model.Source, Notes: model.Notes,
		})
	}
	if effort != nil {
		r := effortRow("worker effort", effort.EffortSettingCellDTO)
		r.Harness, r.Notes = harness, effort.Notes
		rows = append(rows, r)
	}
	if cred != nil {
		resolved := cred.Kind + " credential"
		if cred.AuthMode != nil {
			resolved += " (" + *cred.AuthMode + ")"
		}
		rows = append(rows, settingRow{Setting: "credential", Harness: harness, StoredText: "-", Default: "-", Resolved: resolved, Field: "credential", Source: cred.Source})
	}
	if subs != nil {
		rows = append(rows, settingRow{Setting: "subagents", Harness: harness, StoredText: "-", Default: "-", Resolved: subagentsText(subs)})
	}
	return rows
}

// subagentsText says how the subagents pick a model. At most eight pins are named.
func subagentsText(s *apitypes.EffectiveSubagentsDTO) string {
	out := subagentsBase(s)
	for _, n := range s.Notes[:min(len(s.Notes), 4)] {
		out += " (" + modelSettingSource("note", n) + ")"
	}
	return out
}

func subagentsBase(s *apitypes.EffectiveSubagentsDTO) string {
	lead := "the lead's model: " + orSDKDefault(s.LeadValue)
	if s.Mode != "own_pins" {
		return "all follow " + lead
	}
	if len(s.Pinned) == 0 {
		return "own pins (none set); all follow " + lead
	}
	var parts []string
	for _, p := range s.Pinned[:min(len(s.Pinned), 8)] {
		parts = append(parts, p.Name+"="+p.Model)
	}
	out := "own pins: " + strings.Join(parts, ", ")
	if more := len(s.Pinned) - 8; more > 0 {
		out += fmt.Sprintf(" (+%d more)", more)
	} else if s.PinnedTruncated {
		out += " (more not listed)"
	}
	return out + "; the rest follow " + lead
}

// schedulePreviewLines are the lines after the table: a model pin the harness cannot run, and the
// note that a chosen-when-created harness has no single answer.
func schedulePreviewLines(d *apitypes.ScheduleSettingsPreviewDTO) []string {
	var lines []string
	ignored := func(harness string, m *apitypes.EffectiveModelCellDTO) {
		if m != nil && m.Ignored != nil {
			lines = append(lines, crossCheckPlain(fmt.Sprintf("%s: the model %s from the %s does not run on this harness; the default applies",
				harness, m.Ignored.Value, modelSettingSource("model", m.Ignored.Source))))
		}
	}
	if d.Harness != nil && d.Harness.Value != nil {
		ignored(*d.Harness.Value, d.WorkerModel)
	}
	for _, a := range d.Alternatives[:min(len(d.Alternatives), 2)] {
		ignored(a.Harness, a.WorkerModel)
	}
	if len(d.Alternatives) > 0 {
		lines = append(lines, "The harness is chosen when the run is created; the rows above show each harness it can land on.")
	}
	return lines
}
