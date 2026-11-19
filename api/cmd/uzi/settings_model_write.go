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
// (PRD #3264). Adding a one-value setting is one more entry in modelSettingSpecs; the command,
// the validation and the output read this table. The cross-check pins have their own path
// (runCrossCheckWrite).
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

// crossCheckSetting is the CLI name of the cross-check pin setting (PRD #3264 M2). It is not a
// modelSettingSpec: it has a stage and two members instead of one harness lane and one value.
const crossCheckSetting = "cross-check"

// crossCheckFlags are the cross-check selectors of `uzi settings set|reset`.
type crossCheckFlags struct {
	stage, model, effort string
}

// crossCheckColumns is the header of the cross-check table, the same columns `uzi settings get` prints.
var crossCheckColumns = []string{"STAGE", "HARNESS", "STATUS", "STORED MODEL", "WORKER DEFAULT MODEL",
	"RESOLVED MODEL", "MODEL SOURCE", "STORED EFFORT", "RESOLVED EFFORT", "EFFORT SOURCE"}

// crossCheckEffortLevels are the effort levels a checker pin accepts: the worker levels without runtime.
var crossCheckEffortLevels = slices.DeleteFunc(slices.Clone(effortLevels), func(l string) bool { return l == "runtime" })

func newSettingsSetCmd(env Env, gf *globalFlags) *cobra.Command {
	var harness string
	var cc crossCheckFlags
	cmd := &cobra.Command{
		Use:   "set <setting> [<value>]",
		Short: "Set a model or effort default: worker-model, effort, judge-model, summary-model, judge-runs-on, cross-check",
		Long: "Set one model or effort default for new runs.\n\n" +
			"  uzi settings set worker-model --harness claude <model-id>\n" +
			"  uzi settings set effort --harness codex <low|medium|high|xhigh|max|runtime>\n" +
			"  uzi settings set judge-model --harness codex <model-id>\n" +
			"  uzi settings set summary-model --harness claude <model-id>\n" +
			"  uzi settings set judge-runs-on <same-as-run|claude|codex>\n" +
			"  uzi settings set cross-check --stage plan|code --harness claude|codex [--model <model-id>] [--effort <low|medium|high|xhigh|max>]\n\n" +
			"worker-model, effort, judge-model and summary-model need --harness claude|codex; judge-runs-on\n" +
			"takes none. cross-check pins the checker of one stage and family: it takes no value, needs --stage and\n" +
			"--harness and at least one of --model and --effort, and leaves a member you omit unchanged. A model id\n" +
			"is passed through and the server validates it. Undo with `uzi settings reset`.",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			flags := cmd.Flags()
			if args[0] == crossCheckSetting {
				if len(args) != 1 {
					return uzicli.Exitf(uzicli.ExitUsage, "cross-check takes no value; use --model and --effort")
				}
				return runCrossCheckWrite(cmd, env, gf, cc, harness, flags.Changed("harness"), flags.Changed("stage"),
					flags.Changed("model"), flags.Changed("effort"), false)
			}
			if flags.Changed("stage") || flags.Changed("model") || flags.Changed("effort") {
				return uzicli.Exitf(uzicli.ExitUsage, "--stage, --model and --effort apply only to the cross-check setting")
			}
			if len(args) != 2 {
				return uzicli.Exitf(uzicli.ExitUsage, "usage: uzi settings set <setting> <value>")
			}
			return runModelSettingWrite(cmd, env, gf, args[0], harness, flags.Changed("harness"), &args[1])
		},
	}
	cmd.Flags().StringVar(&harness, "harness", "", "harness the setting applies to: claude or codex")
	cmd.Flags().StringVar(&cc.stage, "stage", "", "cross-check stage: plan or code")
	cmd.Flags().StringVar(&cc.model, "model", "", "cross-check checker model id")
	cmd.Flags().StringVar(&cc.effort, "effort", "", "cross-check checker effort: low, medium, high, xhigh or max")
	return cmd
}

func newSettingsResetCmd(env Env, gf *globalFlags) *cobra.Command {
	var harness string
	var cc crossCheckFlags
	cmd := &cobra.Command{
		Use:   "reset <setting>",
		Short: "Clear a model or effort default back to its inherited value",
		Long: "Clear one model or effort default so it inherits again (judge-runs-on returns to same as the run).\n" +
			"Takes the same settings and --harness as `uzi settings set`.\n\n" +
			"  uzi settings reset cross-check --stage plan|code --harness claude|codex\n\n" +
			"clears both the model and the effort pin of that checker cell: its model follows the worker default and its effort the cross-check default (high).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			flags := cmd.Flags()
			if args[0] == crossCheckSetting {
				return runCrossCheckWrite(cmd, env, gf, cc, harness, flags.Changed("harness"), flags.Changed("stage"), false, false, true)
			}
			if flags.Changed("stage") {
				return uzicli.Exitf(uzicli.ExitUsage, "--stage applies only to the cross-check setting")
			}
			return runModelSettingWrite(cmd, env, gf, args[0], harness, flags.Changed("harness"), nil)
		},
	}
	cmd.Flags().StringVar(&harness, "harness", "", "harness the setting applies to: claude or codex")
	cmd.Flags().StringVar(&cc.stage, "stage", "", "cross-check stage: plan or code")
	return cmd
}

// runCrossCheckWrite validates a cross-check write client-side (exit 2, no request), sends one cell
// holding only the members being changed (a reset sends both as null), then prints that cell.
func runCrossCheckWrite(cmd *cobra.Command, env Env, gf *globalFlags, cc crossCheckFlags, harness string,
	harnessSet, stageSet, modelSet, effortSet, reset bool) error {
	if !stageSet || !slices.Contains([]string{"plan", "code"}, cc.stage) {
		return uzicli.Exitf(uzicli.ExitUsage, "cross-check needs --stage plan or --stage code")
	}
	if !harnessSet || !slices.Contains([]string{"claude", "codex"}, harness) {
		return uzicli.Exitf(uzicli.ExitUsage, "cross-check needs --harness claude or --harness codex")
	}
	members := map[string]*string{}
	if reset {
		members["model"], members["effort"] = nil, nil
	} else {
		if !modelSet && !effortSet {
			return uzicli.Exitf(uzicli.ExitUsage, "cross-check needs --model, --effort or both")
		}
		if modelSet {
			m, err := parseModelID(cc.model)
			if err != nil {
				return uzicli.Exitf(uzicli.ExitUsage, "%v", err)
			}
			members["model"] = m
		}
		if effortSet {
			if !slices.Contains(crossCheckEffortLevels, cc.effort) {
				return uzicli.Exitf(uzicli.ExitUsage, "invalid cross-check effort %q; use %s", cc.effort, strings.Join(crossCheckEffortLevels, ", "))
			}
			e := cc.effort
			members["effort"] = &e
		}
	}
	c, err := env.client(gf)
	if err != nil {
		return err
	}
	res, err := c.PutCrossCheckPins(cmd.Context(), []uzicli.CrossCheckPinWire{{Stage: cc.stage, Harness: harness, Members: members}})
	if err != nil {
		return err
	}
	p := env.printer(gf)
	if p.Format == uzicli.FormatJSON {
		return printModelSettingsJSON(p, res)
	}
	var rows [][]string
	for _, row := range settingsCrossCheckRows(res.Settings) {
		if row[0] == cc.stage && row[1] == harness {
			rows = append(rows, row)
		}
	}
	if len(rows) == 0 {
		p.Println("Saved cross-check " + cc.stage + " (" + harness + ")")
		return nil
	}
	p.Println("Cross-check")
	return p.Table(crossCheckColumns, rows)
}

// printModelSettingsJSON prints the server's {"settings": ...} envelope unchanged, re-encoding the
// decoded settings only when the client kept no raw bytes.
func printModelSettingsJSON(p *uzicli.Printer, res uzicli.ModelSettingsResult) error {
	raw := res.Raw
	if len(raw) == 0 {
		var err error
		if raw, err = json.Marshal(map[string]apitypes.UserSettingsDTO{"settings": res.Settings}); err != nil {
			return err
		}
	}
	return p.JSON(raw)
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
		return printModelSettingsJSON(p, res)
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
