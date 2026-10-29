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
}

var settingsModelColumns = []string{"SETTING", "HARNESS", "STORED", "DEFAULT", "RESOLVED", "SOURCE"}

// modelSettingLabels mirrors fixtures/model-settings/contract.json source_labels
// (go:embed cannot reach the fixtures directory above this module); the CLI test
// pins this table to the fixture in both directions.
var modelSettingLabels = map[[2]string]string{
	{"model", "user_default"}:     "your choice",
	{"model", "instance_default"}: "instance default",
	{"model", "builtin"}:          "built-in",
	{"effort", "user_default"}:    "your choice",
	{"effort", "builtin"}:         "built-in",
	{"effort", "runtime_default"}: "runtime default",
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
// settingRows. At most two cells are read. A runtime-default cell has no resolved level:
// Claude reports no value at all, so its RESOLVED says so.
func effortSettingRows(cells []apitypes.EffortSettingCellDTO) []settingRow {
	var rows []settingRow
	for _, c := range cells[:min(len(cells), 2)] {
		resolved := ""
		switch {
		case c.ResolvedEffort != nil:
			resolved = *c.ResolvedEffort
		case c.Harness == "claude":
			resolved = "runtime default (value not reported)"
		default:
			resolved = "runtime default"
		}
		rows = append(rows, settingRow{
			Setting: "effort", Harness: c.Harness, Stored: c.Effort,
			Default: c.DefaultEffort, Resolved: resolved,
			Field: "effort", Source: c.Source,
		})
	}
	return rows
}

// settingRowStored renders the STORED column. Only an effort row holding exactly the
// Runtime default token shows it as the label; every other row, including a model pin
// that happens to be named runtime, keeps the Pin formatting.
func settingRowStored(r settingRow) string {
	if r.Field == "effort" && r.Stored != nil && *r.Stored == "runtime" {
		return modelSettingSource("effort", "runtime_default")
	}
	return settingsStoredPin(r.Stored)
}

// settingRowsTable renders neutral rows; every cell passes through crossCheckPlain.
func settingRowsTable(rows []settingRow) [][]string {
	out := make([][]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, []string{
			crossCheckPlain(r.Setting), crossCheckPlain(r.Harness), settingRowStored(r),
			crossCheckPlain(r.Default), crossCheckPlain(r.Resolved),
			crossCheckPlain(modelSettingSource(r.Field, r.Source)),
		})
	}
	return out
}

// settingsModelRows collects every setting family's rows for `uzi settings get`.
func settingsModelRows(s apitypes.UserSettingsDTO) []settingRow {
	return append(modelSettingRows("judge", s.JudgeModels), effortSettingRows(s.EffortSettings)...)
}
