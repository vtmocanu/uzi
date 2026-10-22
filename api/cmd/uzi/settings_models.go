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

// settingRowsTable renders neutral rows; every cell passes through crossCheckPlain.
func settingRowsTable(rows []settingRow) [][]string {
	out := make([][]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, []string{
			crossCheckPlain(r.Setting), crossCheckPlain(r.Harness), settingsStoredPin(r.Stored),
			crossCheckPlain(r.Default), crossCheckPlain(r.Resolved),
			crossCheckPlain(modelSettingSource(r.Field, r.Source)),
		})
	}
	return out
}

// settingsModelRows collects every setting family's rows for `uzi settings get`.
func settingsModelRows(s apitypes.UserSettingsDTO) []settingRow {
	return modelSettingRows("judge", s.JudgeModels)
}
