package handler

import (
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/agenttmpl"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/settings"
)

// judgeModelCells builds the judge setting's cells (PRD #3245), always ordered
// claude then codex. userClaude/userCodex are the stored user lanes (nil = Default);
// instance maps the instance model keys to their InstanceModels values. It reads no
// settings itself: the caller passes its own snapshot, so every surface resolves
// from one read.
func judgeModelCells(userClaude, userCodex *string, instance map[string]string) []apitypes.ModelSettingCellDTO {
	return []apitypes.ModelSettingCellDTO{
		judgeModelCell("claude", userClaude, instance[settings.KeyJudgeModel]),
		judgeModelCell("codex", userCodex, instance[settings.KeyCodexJudgeModel]),
	}
}

func judgeModelCell(harness string, user *string, instance string) apitypes.ModelSettingCellDTO {
	stored := ""
	if user != nil {
		stored = *user
	}
	r := agenttmpl.ResolveJudgeModel(harness, stored, instance)
	return apitypes.ModelSettingCellDTO{
		Harness:       harness,
		Model:         user,
		DefaultModel:  r.DefaultModel,
		DefaultSource: r.DefaultSource,
		ResolvedModel: r.Model,
		Source:        r.Source,
	}
}

// effortSettingCells builds the ordinary effort setting's cells (PRD #3247), always
// ordered claude then codex, from the stored preference tokens of one row read.
func effortSettingCells(claude, codex *string) []apitypes.EffortSettingCellDTO {
	return []apitypes.EffortSettingCellDTO{
		effortSettingCell("claude", claude),
		effortSettingCell("codex", codex),
	}
}

func effortSettingCell(harness string, stored *string) apitypes.EffortSettingCellDTO {
	def := agenttmpl.ResolveHarnessEffort(harness, nil)
	r := agenttmpl.ResolveHarnessEffort(harness, stored)
	cell := apitypes.EffortSettingCellDTO{
		Harness:       harness,
		Effort:        stored,
		DefaultEffort: def.Level,
		DefaultSource: def.Source,
		Source:        r.Source,
		Mode:          r.Mode,
	}
	if r.Mode == agenttmpl.EffortModeExplicit {
		level := r.Level
		cell.ResolvedEffort = &level
	}
	return cell
}

// validateCodexJudgeModel validates a user Codex judge lane value: nil or blank clears
// to NULL; anything else must be a curated Codex id after trimming, and is stored trimmed.
func validateCodexJudgeModel(raw *string) (pgtype.Text, error) {
	if raw == nil {
		return pgtype.Text{}, nil
	}
	m, err := agenttmpl.ValidateModel(*raw)
	if err != nil {
		return pgtype.Text{}, err
	}
	if m == "" {
		return pgtype.Text{}, nil
	}
	if !agenttmpl.CuratedCodexModels[m] {
		return pgtype.Text{}, fmt.Errorf("%q is not a supported Codex model", m)
	}
	return pgtype.Text{String: m, Valid: true}, nil
}

// validateClaudeJudgeModel validates the user Claude judge lane: the shared model rules
// plus a refusal of any Codex role id.
func validateClaudeJudgeModel(raw *string) (pgtype.Text, error) {
	if raw == nil {
		return pgtype.Text{}, nil
	}
	m, err := agenttmpl.ValidateClaudeLaneModel(*raw)
	if err != nil {
		return pgtype.Text{}, err
	}
	if m == "" {
		return pgtype.Text{}, nil
	}
	return pgtype.Text{String: m, Valid: true}, nil
}
