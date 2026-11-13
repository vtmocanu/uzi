package main

import "github.com/vtmocanu/uzi/api/internal/apitypes"

// settingRow is the neutral row of the "Model settings" table of `uzi settings get`
// (PRD #3245). Every setting family adapts its cells into settingRow and renders
// through settingRowsTable, so there is one column set and never a second table.
// Field names the (field, token) label table that Source is looked up in.
type settingRow struct {
	Setting, Harness string
	Stored           *string
	Default          string
	Resolved         string
	Field, Source    string
	// Notes are the note codes of an effective-settings cell, rendered after the source. A row
	// with no Field (a refusal) renders no source.
	Notes []string
	// StoredText replaces the STORED cell for a row whose setting has no stored value (a start
	// choice is made per run, not saved).
	StoredText string
}

var settingsModelColumns = []string{"SETTING", "HARNESS", "STORED", "DEFAULT", "RESOLVED", "SOURCE"}

// modelSettingLabels mirrors fixtures/model-settings/contract.json source_labels
// (go:embed cannot reach the fixtures directory above this module); the CLI test
// pins this table to the fixture in both directions.
var modelSettingLabels = map[[2]string]string{
	{"model", "user_default"}:                   "your choice",
	{"model", "instance_default"}:               "instance default",
	{"model", "builtin"}:                        "built-in",
	{"effort", "user_default"}:                  "your choice",
	{"effort", "builtin"}:                       "built-in",
	{"effort", "runtime_default"}:               "runtime default",
	{"effort", "catalog_default"}:               "catalog default",
	{"model", "schedule_pin"}:                   "schedule pin",
	{"model", "template"}:                       "lead template",
	{"model", "cross_check_pin"}:                "checker pin",
	{"model", "cross_check_worker_default"}:     "worker default",
	{"effort", "cross_check_pin"}:               "checker pin",
	{"effort", "cross_check_default"}:           "cross-check default",
	{"harness", "run_override"}:                 "your choice for this run",
	{"harness", "schedule_pin"}:                 "schedule pin",
	{"harness", "user_default"}:                 "your default harness",
	{"harness", "automatic"}:                    "automatic",
	{"harness", "chosen_at_create"}:             "chosen when the run is created",
	{"judge_harness", "user_default"}:           "your setting",
	{"judge_harness", "same_as_run"}:            "same as the run",
	{"credential", "chosen_at_create"}:          "chosen when the run is created",
	{"credential", "claim_selection"}:           "chosen when the run starts",
	{"credential", "auto_selection"}:            "automatic selection",
	{"credential", "owner_default"}:             "your default credential",
	{"credential", "token_override"}:            "token chosen for this run",
	{"credential", "codex_default_credential"}:  "your default Codex credential",
	{"note", "schedule_pin_ignored"}:            "the schedule's model pin does not apply to this harness",
	{"note", "sdk_default"}:                     "the SDK decides",
	{"note", "not_configurable"}:                "not configurable",
	{"note", "not_sent"}:                        "not sent",
	{"note", "not_used"}:                        "not used: the judge runs on the other harness",
	{"note", "user_default_harness_unusable"}:   "your default harness has no usable credential",
	{"note", "issue_labels_may_select_harness"}: "issue labels may select the harness",
	{"note", "repo_agents_not_predicted"}:       "repository agents are not predicted",
	{"note", "token_fixed"}:                     "the token is fixed",
	{"note", "instance_settings_unavailable"}:   "instance settings are unavailable",
	{"note", "automatic_weekly_reset"}:          "chosen by the soonest weekly reset",
	{"note", "overflow"}:                        "may move to the other harness near its limit",
}

// modelSettingSource labels a provenance token for a settings field. An unknown
// (field, token) pair renders "unknown": a server value cannot assert a label.
func modelSettingSource(field, token string) string {
	if label, ok := modelSettingLabels[[2]string{field, token}]; ok {
		return label
	}
	return "unknown"
}

// modelSettingRows adapts a model setting's cells (field "model") into settingRows
// for the named setting. At most two cells are read, so a hostile response cannot
// inflate the table.
func modelSettingRows(setting string, cells []apitypes.ModelSettingCellDTO) []settingRow {
	var rows []settingRow
	for _, c := range cells[:min(len(cells), 2)] {
		rows = append(rows, settingRow{
			Setting: setting, Harness: c.Harness, Stored: c.Model,
			Default: c.DefaultModel, Resolved: c.ResolvedModel,
			Field: "model", Source: c.Source,
		})
	}
	return rows
}

// effortSettingRows adapts the ordinary effort cells (PRD #3247, field "effort") into
// settingRows. At most two cells are read.
func effortSettingRows(cells []apitypes.EffortSettingCellDTO) []settingRow {
	var rows []settingRow
	for _, c := range cells[:min(len(cells), 2)] {
		rows = append(rows, effortRow("effort", c))
	}
	return rows
}

// effortRow adapts one effort cell. A runtime-default cell has no resolved level: Claude reports
// no value at all, so its RESOLVED says so, and Codex shows the catalog default.
func effortRow(setting string, c apitypes.EffortSettingCellDTO) settingRow {
	resolved := ""
	switch {
	case c.Mode == "runtime_default" && c.Harness == "claude":
		resolved = "runtime default (value not reported)"
	case c.Mode == "runtime_default":
		resolved = codexRuntimeDefaultResolved(c.CatalogDefault)
	case c.ResolvedEffort != nil:
		resolved = *c.ResolvedEffort
	default:
		resolved = "unknown"
	}
	return settingRow{
		Setting: setting, Harness: c.Harness, Stored: c.Effort,
		Default: c.DefaultEffort, Resolved: resolved,
		Field: "effort", Source: c.Source,
	}
}

// codexRuntimeDefaultResolved renders the codex Runtime default RESOLVED text (issue
// #3251) with the catalog default the server reported. Every server string is passed
// through crossCheckPlain; an absent or unrecognised catalog state reads unknown, and
// a known/varies state without its value fields does too.
func codexRuntimeDefaultResolved(cd *apitypes.CatalogDefaultDTO) string {
	label := modelSettingSource("effort", "runtime_default")
	catalog := modelSettingSource("effort", "catalog_default")
	unknown := label + " (" + catalog + " unknown)"
	if cd == nil || (cd.State != "known" && cd.State != "varies") ||
		cd.Effort == nil || cd.Model == nil || cd.RuntimeVersion == nil {
		return unknown
	}
	// PRD D13: Codex 0.164.0 sends every stored level as-is except ultra, which
	// resolve_reasoning_effort replaces before the request.
	effort := crossCheckPlain(*cd.Effort)
	switch effort {
	case "none":
		effort = "none, no reasoning level requested"
	case "ultra":
		effort = "ultra, Codex substitutes another level"
	}
	out := label + " (" + catalog + ": " + effort + ", " + crossCheckPlain(*cd.Model) + ", Codex " + crossCheckPlain(*cd.RuntimeVersion) + ")"
	if cd.State == "varies" {
		out += " · may differ on workers still on another version"
	}
	return out
}

// settingRowStored renders the STORED column. Only an effort row holding exactly the
// Runtime default token shows it as the label; every other row, including a model pin
// that happens to be named runtime, keeps the Pin formatting.
func settingRowStored(r settingRow) string {
	if r.StoredText != "" {
		return r.StoredText
	}
	if r.Field == "effort" && r.Stored != nil && *r.Stored == "runtime" {
		return modelSettingSource("effort", "runtime_default")
	}
	return settingsStoredPin(r.Stored)
}

// settingRowSource renders the SOURCE column: the (field, token) label, then the labelled notes
// (at most four). A row with no field has no provenance and shows a dash.
func settingRowSource(r settingRow) string {
	if r.Field == "" {
		return "-"
	}
	out := modelSettingSource(r.Field, r.Source)
	for _, n := range r.Notes[:min(len(r.Notes), 4)] {
		out += " · " + modelSettingSource("note", n)
	}
	return crossCheckPlain(out)
}

// settingRowsTable renders neutral rows; every cell passes through crossCheckPlain.
func settingRowsTable(rows []settingRow) [][]string {
	out := make([][]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, []string{
			crossCheckPlain(r.Setting), crossCheckPlain(r.Harness), settingRowStored(r),
			crossCheckPlain(r.Default), crossCheckPlain(r.Resolved), settingRowSource(r),
		})
	}
	return out
}

// settingsModelRows collects every setting family's rows for `uzi settings get`.
func settingsModelRows(s apitypes.UserSettingsDTO) []settingRow {
	rows := append(modelSettingRows("judge", s.JudgeModels), modelSettingRows("summary", s.SummaryModels)...)
	return append(rows, effortSettingRows(s.EffortSettings)...)
}
