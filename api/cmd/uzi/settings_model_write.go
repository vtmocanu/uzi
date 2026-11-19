package main

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// modelSettingSpec describes one settable model or effort setting of `uzi settings set|reset`
// (PRD #3264). Adding a setting (the cross-check pins, #3264 M2) is one more entry in
// modelSettingSpecs; the command, the validation and the output read this table.
type modelSettingSpec struct {
	// name is the CLI setting name.
	name string
	// fields maps a harness to the API field. A setting with no harness lane has the single key "".
	fields map[string]string
	// parse validates a value client-side and returns the patch value; nil clears the setting.
	parse func(value string) (*string, error)
	// rowLabel is the SETTING cell of the effective "Model settings" table; fallbackLabel is the
	// SETTING cell of the stored-only table (settingsModelRows), "" when that table has no row.
	rowLabel, fallbackLabel string
}

var effortLevels = []string{"low", "medium", "high", "xhigh", "max", "runtime"}

func parseModelID(v string) (*string, error) {
	if strings.TrimSpace(v) == "" {
		return nil, fmt.Errorf("the model id must not be empty")
	}
	return &v, nil
}

func parseEffort(v string) (*string, error) {
	if !slices.Contains(effortLevels, v) {
		return nil, fmt.Errorf("invalid effort %q; use %s", v, strings.Join(effortLevels, ", "))
	}
	return &v, nil
}

func parseJudgeRunsOn(v string) (*string, error) {
	switch v {
	case "same-as-run":
		return nil, nil
	case "claude", "codex":
		return &v, nil
	}
	return nil, fmt.Errorf("invalid value %q; use same-as-run, claude or codex", v)
}

var modelSettingSpecs = []modelSettingSpec{
	{name: "worker-model", fields: map[string]string{"claude": "default_claude_model", "codex": "default_codex_model"},
		parse: parseModelID, rowLabel: effectiveModelLabels["worker_model"]},
	{name: "effort", fields: map[string]string{"claude": "default_effort", "codex": "default_codex_effort"},
		parse: parseEffort, rowLabel: effectiveEffortLabels["worker_effort"], fallbackLabel: "effort"},
	{name: "judge-model", fields: map[string]string{"claude": "judge_model", "codex": "codex_judge_model"},
		parse: parseModelID, rowLabel: "judge", fallbackLabel: "judge"},
	{name: "summary-model", fields: map[string]string{"claude": "summary_model", "codex": "codex_summary_model"},
		parse: parseModelID, rowLabel: "summary", fallbackLabel: "summary"},
	{name: "judge-runs-on", fields: map[string]string{"": "judge_runs_on"}, parse: parseJudgeRunsOn},
}

func (s modelSettingSpec) perHarness() bool { return len(s.fields) > 1 }

// lookupModelSetting finds a spec or returns a usage error naming the valid settings.
func lookupModelSetting(name string) (modelSettingSpec, error) {
	names := make([]string, 0, len(modelSettingSpecs))
	for _, s := range modelSettingSpecs {
		if s.name == name {
			return s, nil
		}
		names = append(names, s.name)
	}
	return modelSettingSpec{}, uzicli.Exitf(uzicli.ExitUsage, "unknown setting %q; use one of %s", name, strings.Join(names, ", "))
}

// apiField resolves the harness selector to the API field, refusing a missing or invalid
// --harness on a per-harness setting and any --harness on one without a harness lane.
func (s modelSettingSpec) apiField(harness string, harnessSet bool) (string, error) {
	if !s.perHarness() {
		if harnessSet {
			return "", uzicli.Exitf(uzicli.ExitUsage, "%s has no harness lane; omit --harness", s.name)
		}
		return s.fields[""], nil
	}
	field, ok := s.fields[harness]
	if !ok {
		return "", uzicli.Exitf(uzicli.ExitUsage, "%s needs --harness claude or --harness codex", s.name)
	}
	return field, nil
}

func newSettingsSetCmd(env Env, gf *globalFlags) *cobra.Command {
	var harness string
	cmd := &cobra.Command{
		Use:   "set <setting> <value>",
		Short: "Set a model or effort default: worker-model, effort, judge-model, summary-model, judge-runs-on",
		Long: "Set one model or effort default for new runs.\n\n" +
			"  uzi settings set worker-model --harness claude <model-id>\n" +
			"  uzi settings set effort --harness codex <low|medium|high|xhigh|max|runtime>\n" +
			"  uzi settings set judge-model --harness codex <model-id>\n" +
			"  uzi settings set summary-model --harness claude <model-id>\n" +
			"  uzi settings set judge-runs-on <same-as-run|claude|codex>\n\n" +
			"worker-model, effort, judge-model and summary-model need --harness claude|codex; judge-runs-on\n" +
			"takes none. A model id is passed through and the server validates it. Undo with `uzi settings reset`.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runModelSettingWrite(cmd, env, gf, args[0], harness, cmd.Flags().Changed("harness"), &args[1])
		},
	}
	cmd.Flags().StringVar(&harness, "harness", "", "harness the setting applies to: claude or codex")
	return cmd
}

func newSettingsResetCmd(env Env, gf *globalFlags) *cobra.Command {
	var harness string
	cmd := &cobra.Command{
		Use:   "reset <setting>",
		Short: "Clear a model or effort default back to its inherited value",
		Long: "Clear one model or effort default so it inherits again (judge-runs-on returns to same as the run).\n" +
			"Takes the same settings and --harness as `uzi settings set`.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runModelSettingWrite(cmd, env, gf, args[0], harness, cmd.Flags().Changed("harness"), nil)
		},
	}
	cmd.Flags().StringVar(&harness, "harness", "", "harness the setting applies to: claude or codex")
	return cmd
}

// runModelSettingWrite validates everything client-side (exit 2, no request), sends the one-field
// patch, then prints the change. value nil is a reset.
func runModelSettingWrite(cmd *cobra.Command, env Env, gf *globalFlags, setting, harness string, harnessSet bool, value *string) error {
	spec, err := lookupModelSetting(setting)
	if err != nil {
		return err
	}
	field, err := spec.apiField(harness, harnessSet)
	if err != nil {
		return err
	}
	var send *string
	if value != nil {
		if send, err = spec.parse(*value); err != nil {
			return uzicli.Exitf(uzicli.ExitUsage, "%v", err)
		}
	}
	c, err := env.client(gf)
	if err != nil {
		return err
	}
	res, err := c.PutModelSettings(cmd.Context(), map[string]*string{field: send})
	if err != nil {
		return err
	}
	p := env.printer(gf)
	if p.Format == uzicli.FormatJSON {
		raw := res.Raw
		if len(raw) == 0 {
			if raw, err = json.Marshal(map[string]apitypes.UserSettingsDTO{"settings": res.Settings}); err != nil {
				return err
			}
		}
		return p.JSON(raw)
	}
	// The write is durable once the PUT answers 200, so a failed effective readback never fails
	// the command: readEffectiveSettings reports it on stderr and the stored values are shown.
	effective := readEffectiveSettings(cmd.Context(), c, env.Stderr)
	if !spec.perHarness() {
		p.Println(settingsJudgeRunsOnLine(res.Settings, effective))
	} else {
		printModelSettingChange(p, spec, harness, field, res.Settings, effective)
	}
	if effective == nil {
		p.Println("Saved; the effective view is unavailable, so only stored values are shown.")
	}
	return nil
}

// printModelSettingChange shows the changed row of the "Model settings" table, from the effective
// view when there is one and the stored-only table otherwise. A setting the stored-only table has no
// row for prints its stored value.
func printModelSettingChange(p *uzicli.Printer, spec modelSettingSpec, harness, field string, s apitypes.UserSettingsDTO, effective *apitypes.EffectiveSettingsDTO) {
	var all []settingRow
	label := spec.fallbackLabel
	if effective != nil {
		all, label = effectiveSettingsRows(s, effective), spec.rowLabel
	} else {
		all = settingsModelRows(s)
	}
	var rows []settingRow
	for _, r := range all {
		if label != "" && r.Setting == label && r.Harness == harness {
			rows = append(rows, r)
		}
	}
	if len(rows) > 0 {
		p.Println("Model settings")
		_ = p.Table(settingsModelColumns, settingRowsTable(rows))
		return
	}
	stored := "default (not set)"
	if v := storedModelSetting(s, field); v != nil {
		stored = *v
	}
	p.Println("Saved " + spec.name + " (" + harness + "): " + stored)
}

// storedModelSetting reads the stored value of an API field from the settings read.
func storedModelSetting(s apitypes.UserSettingsDTO, field string) *string {
	switch field {
	case "default_claude_model":
		return s.DefaultClaudeModel
	case "default_codex_model":
		return s.DefaultCodexModel
	case "default_effort":
		return s.DefaultEffort
	case "default_codex_effort":
		return s.DefaultCodexEffort
	case "judge_model":
		return s.JudgeModel
	case "codex_judge_model":
		return s.CodexJudgeModel
	case "summary_model":
		return s.SummaryModel
	case "codex_summary_model":
		return s.CodexSummaryModel
	case "judge_runs_on":
		return s.JudgeRunsOn
	}
	return nil
}
