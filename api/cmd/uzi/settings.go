package main

import (
	"github.com/spf13/cobra"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func newSettingsCmd(env Env, gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use: "settings", Short: "Read your account settings", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(&cobra.Command{
		Use: "get", Short: "Show account settings and plan cross-check pins", Args: cobra.NoArgs,
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
			return p.Table([]string{"STAGE", "HARNESS", "STATUS", "STORED MODEL", "WORKER DEFAULT MODEL",
				"RESOLVED MODEL", "MODEL SOURCE", "STORED EFFORT", "RESOLVED EFFORT", "EFFORT SOURCE"},
				settingsCrossCheckRows(settings))
		},
	})
	return cmd
}

func settingsCrossCheckRows(s apitypes.UserSettingsDTO) [][]string {
	var rows [][]string
	// Only the two plan cells are supported. Inspect at most twenty response
	// cells and display each family once; malformed responses stay bounded.
	for _, harness := range []string{"claude", "codex"} {
		for _, cell := range s.CrossCheckPins[:min(len(s.CrossCheckPins), 20)] {
			if cell.Stage != "plan" || cell.Harness != harness {
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
				"plan", harness, status, settingsStoredPin(cell.Model),
				crossCheckPlain(strOr(cell.WorkerDefaultModel, modelDefault)),
				crossCheckPlain(strOr(cell.ResolvedModel, modelDefault)),
				crossCheckSource(&cell.ModelSource), settingsStoredPin(cell.Effort),
				crossCheckPlain(cell.ResolvedEffort), crossCheckSource(&cell.EffortSource),
			})
			break
		}
	}
	return rows
}

func settingsStoredPin(pin *string) string {
	if pin == nil {
		return "Default"
	}
	return crossCheckPlain("Pin · " + *pin)
}
