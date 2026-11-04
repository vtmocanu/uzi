package main

import (
	"github.com/spf13/cobra"

	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func newSettingsHarnessOverflowCmd(env Env, gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "harness-overflow on|off",
		Short: "Set cross-harness overflow for new runs",
		Long: "Overflow to the other harness when my default is near its limits. Applies only with an\n" +
			"explicit Claude or Codex default harness and is inactive when the default is automatic.\n" +
			"Applies at creation\n" +
			"to implicit issue runs started manually, by autopilot or by a schedule, when both\n" +
			"harnesses are usable. Explicit harnesses, model pins, Anthropic credential overrides\n" +
			"and either cross-check setting prevent overflow. Existing runs never switch.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if args[0] != "on" && args[0] != "off" {
				return uzicli.Exitf(uzicli.ExitUsage, "invalid value %q; use on or off", args[0])
			}
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			stored, err := c.SetHarnessOverflowEnabled(cmd.Context(), args[0] == "on")
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(map[string]bool{"harness_overflow_enabled": stored})
			}
			// Overflow only acts with an explicit default harness, so read the
			// default back to say whether the stored value is active. A failed read
			// still reports the stored value and surfaces the error.
			settings, readErr := c.GetMySettings(cmd.Context())
			if readErr != nil {
				explicit := ""
				p.Println(harnessOverflowLine(stored, &explicit))
				return readErr
			}
			p.Println(harnessOverflowLine(stored, settings.DefaultHarness))
			return nil
		},
	}
}

// harnessOverflowLine renders the overflow switch; a nil default harness means
// automatic, where overflow has no effect.
func harnessOverflowLine(enabled bool, defaultHarness *string) string {
	state := "off"
	if enabled {
		state = "on"
	}
	if defaultHarness == nil {
		return "Harness overflow: " + state + " (inactive: default harness is automatic)"
	}
	return "Harness overflow: " + state + " (new runs, at creation)"
}
