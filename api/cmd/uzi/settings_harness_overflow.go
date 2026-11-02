package main

import (
	"github.com/spf13/cobra"

	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func newSettingsHarnessOverflowCmd(env Env, gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "harness-overflow on|off",
		Short: "Set cross-harness overflow for new runs",
		Long: "Overflow to the other harness when my default is near its limits. Applies at creation\n" +
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
			p.Println(harnessOverflowLine(stored))
			return nil
		},
	}
}

func harnessOverflowLine(enabled bool) string {
	if enabled {
		return "Harness overflow: on (new runs, at creation)"
	}
	return "Harness overflow: off (new runs, at creation)"
}
