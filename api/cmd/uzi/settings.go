package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func newSettingsCmd(env Env, gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use: "settings", Short: "Read account settings; set usage reserve, Codex account mode and harness overflow", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	var scheduleID string
	get := &cobra.Command{
		Use: "get", Short: "Show account settings, cross-check pins and model settings; --schedule previews a schedule's fire", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			settings, err := c.GetMySettings(cmd.Context())
			if err != nil {
				return err
			}
			// A schedule the caller asked about must exist: a 404 (foreign, missing or an older
			// server) fails the command before anything is printed.
			var schedule *apitypes.ScheduleSettingsPreviewDTO
			if scheduleID != "" {
				if schedule, err = readSchedulePreview(cmd.Context(), c, scheduleID, env.Stderr); err != nil {
					return err
				}
			}
			p := env.printer(gf)
			effective := readEffectiveSettings(cmd.Context(), c, env.Stderr)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(settingsGetJSON{UserSettingsDTO: settings, Effective: effective, SchedulePreview: schedule})
			}
			if err := p.Table([]string{"STAGE", "HARNESS", "STATUS", "STORED MODEL", "WORKER DEFAULT MODEL",
				"RESOLVED MODEL", "MODEL SOURCE", "STORED EFFORT", "RESOLVED EFFORT", "EFFORT SOURCE"},
				settingsCrossCheckRows(settings)); err != nil {
				return err
			}
			// PRD #3245: one table for every model setting, after the cross-check table.
			rows := settingsModelRows(settings)
			if effective != nil {
				// PRD #3254: the effective view carries the worker, task-review and start rows;
				// each setting prints once.
				rows = effectiveSettingsRows(settings, effective)
			}
			if len(rows) > 0 {
				p.Println("Model settings")
				if err := p.Table(settingsModelColumns, settingRowsTable(rows)); err != nil {
					return err
				}
			}
			if effective != nil {
				for _, line := range effectiveAlternativeLines(effective) {
					p.Println(line)
				}
			}
			if schedule != nil {
				p.Println("If fired and claimed now (schedule " + crossCheckPlain(scheduleID) + ")")
				if err := p.Table(settingsModelColumns, settingRowsTable(schedulePreviewRows(schedule))); err != nil {
					return err
				}
				for _, line := range schedulePreviewLines(schedule) {
					p.Println(line)
				}
			}
			// PRD #2603: nil means the per-user switch was never set, which reads as on.
			p.Println(settingsNowSummaryLine(settings))
			// PRD #3522: the weekly usage reserve, off when no threshold is set.
			p.Println(settingsReserveLine(settings))
			// Issue #3719: how new Codex runs choose a login.
			p.Println(settingsCodexModeLine(settings))
			p.Println(settingsDefaultHarnessLine(settings.DefaultHarness))
			p.Println(harnessOverflowLine(settings.HarnessOverflowEnabled, settings.DefaultHarness))
			p.Println(settingsJudgeRunsOnLine(settings, effective))
			return nil
		},
	}
	get.Flags().StringVar(&scheduleID, "schedule", "", "preview what firing this saved schedule now would use (schedule id)")
	cmd.AddCommand(get)
	cmd.AddCommand(newSettingsReserveCmd(env, gf))
	cmd.AddCommand(newSettingsCodexModeCmd(env, gf))
	cmd.AddCommand(newSettingsHarnessOverflowCmd(env, gf))
	return cmd
}

// settingsDefaultHarnessLine renders the per-user default harness; nil is automatic.
func settingsDefaultHarnessLine(h *string) string {
	if h == nil {
		return "Default harness: automatic (chosen per run by weekly limits when both harnesses are usable)"
	}
	return "Default harness: " + cellText(*h)
}

// settingsJudgeRunsOnLine renders the per-user "Judge runs on" setting (PRD #3257). The
// effective view's judge_harness cell is preferred (it carries the source token); without
// it the stored setting is shown, where null is the default, "same as the run".
func settingsJudgeRunsOnLine(s apitypes.UserSettingsDTO, effective *apitypes.EffectiveSettingsDTO) string {
	value, source := s.JudgeRunsOn, "user_default"
	if effective != nil {
		value, source = effective.JudgeHarness.Value, effective.JudgeHarness.Source
	}
	if value == nil {
		return "Judge runs on: Same as the run (default)"
	}
	name := cellText(*value)
	switch *value {
	case "claude":
		name = "Claude"
	case "codex":
		name = "Codex"
	}
	return "Judge runs on: " + name + " (" + modelSettingSource("judge_harness", source) + ")"
}

func settingsCrossCheckRows(s apitypes.UserSettingsDTO) [][]string {
	var rows [][]string
	// Inspect at most twenty response cells and display each stage/family once.
	// Legacy responses contain only plan cells; absent cells are not invented.
	for _, stage := range []string{"plan", "code"} {
		for _, harness := range []string{"claude", "codex"} {
			for _, cell := range s.CrossCheckPins[:min(len(s.CrossCheckPins), 20)] {
				if cell.Stage != stage || cell.Harness != harness {
					continue
				}
				status := "active"
				if !cell.Active {
					status = "inactive"
				}
				modelDefault := "unknown"
				if harness == "claude" {
					modelDefault = "SDK/account default"
				}
				rows = append(rows, []string{
					stage, harness, status, settingsStoredPin(cell.Model),
					crossCheckPlain(strOr(cell.WorkerDefaultModel, modelDefault)),
					crossCheckPlain(strOr(cell.ResolvedModel, modelDefault)),
					crossCheckSource(&cell.ModelSource), settingsStoredPin(cell.Effort),
					crossCheckPlain(cell.ResolvedEffort), crossCheckEffortSource(&cell.EffortSource),
				})
				break
			}
		}
	}
	return rows
}

func settingsNowSummaryLine(s apitypes.UserSettingsDTO) string {
	switch {
	case s.NowSummaryEnabled == nil:
		return "Now summary: on (default)"
	case *s.NowSummaryEnabled:
		return "Now summary: on"
	}
	return "Now summary: off"
}

func settingsStoredPin(pin *string) string {
	if pin == nil {
		return "Default"
	}
	return crossCheckPlain("Pin · " + *pin)
}

// readEffectiveSettings reads the effective-settings view without letting it fail the command:
// `settings get` is the settings read, which already succeeded. A 404 means an older server and
// any other failure (5xx, network) is reported on stderr; either way the view is nil and the exit
// code stays 0. Server text on stderr passes crossCheckPlain.
func readEffectiveSettings(ctx context.Context, c uzicli.Client, stderr io.Writer) *apitypes.EffectiveSettingsDTO {
	e, err := c.GetEffectiveSettings(ctx)
	if err == nil {
		return &e
	}
	var ee *uzicli.ExitError
	if errors.As(err, &ee) && ee.Code == uzicli.ExitNotFound {
		_, _ = fmt.Fprintln(stderr, "Effective settings: not available on this server")
		return nil
	}
	_, _ = fmt.Fprintf(stderr, "Effective settings: unavailable (%s)\n", crossCheckPlain(err.Error()))
	return nil
}
