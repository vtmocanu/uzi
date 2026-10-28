package main

import (
	"github.com/spf13/cobra"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func newSettingsCmd(env Env, gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use: "settings", Short: "Read your account settings and set the weekly usage reserve", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(&cobra.Command{
		Use: "get", Short: "Show account settings, cross-check pins and model settings", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			settings, err := c.GetMySettings(cmd.Context())
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(settings)
			}
			if err := p.Table([]string{"STAGE", "HARNESS", "STATUS", "STORED MODEL", "WORKER DEFAULT MODEL",
				"RESOLVED MODEL", "MODEL SOURCE", "STORED EFFORT", "RESOLVED EFFORT", "EFFORT SOURCE"},
				settingsCrossCheckRows(settings)); err != nil {
				return err
			}
			// PRD #3245: one table for every model setting, after the cross-check table.
			if rows := settingsModelRows(settings); len(rows) > 0 {
				p.Println("Model settings")
				if err := p.Table(settingsModelColumns, settingRowsTable(rows)); err != nil {
					return err
				}
			}
			// PRD #2603: nil means the per-user switch was never set, which reads as on.
			p.Println(settingsNowSummaryLine(settings))
			// PRD #3522: the weekly usage reserve, off when no threshold is set.
			p.Println(settingsReserveLine(settings))
			return nil
		},
	})
	cmd.AddCommand(newSettingsReserveCmd(env, gf))
	return cmd
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
					crossCheckPlain(cell.ResolvedEffort), crossCheckSource(&cell.EffortSource),
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
