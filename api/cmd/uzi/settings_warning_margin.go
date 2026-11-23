package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// newSettingsWarningMarginCmd is `uzi settings warning-margin <0..50>` (PRD #4123): how many
// percentage points before a weekly limit uzi starts warning.
func newSettingsWarningMarginCmd(env Env, gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "warning-margin <0..50|off>",
		Short: "Set the weekly-limit warning margin",
		Long: "Set the weekly-limit warning margin: how many percentage points before a weekly\n" +
			"limit uzi starts warning you (early warnings on runs and one Slack DM per weekly period). The value is an integer from 0\n" +
			"to 50. 0 (or off) turns early warnings and the Slack warning off;\n" +
			"the explanation on a run parked by a weekly limit stays.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pts, err := parseWarningMargin(args[0])
			if err != nil {
				return uzicli.Exitf(uzicli.ExitUsage, "%v", err)
			}
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			saved, err := c.SetWeeklyWarningMargin(cmd.Context(), pts)
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(map[string]any{"weekly_warning_margin_pts": saved})
			}
			p.Println(warningMarginLine(saved))
			return nil
		},
	}
}

// parseWarningMargin accepts an integer 0..50, or off as 0. The server revalidates; this
// only gives the early, readable message before any request.
func parseWarningMargin(s string) (int, error) {
	t := strings.TrimSpace(s)
	if strings.EqualFold(t, "off") {
		return 0, nil
	}
	n, err := strconv.Atoi(t)
	if err != nil || n < 0 || n > 50 {
		return 0, fmt.Errorf("invalid warning margin %q: use an integer from 0 to 50, or off", s)
	}
	return n, nil
}

// warningMarginLine renders the margin for `settings get` and `settings warning-margin`.
func warningMarginLine(pts int) string {
	if pts == 0 {
		return "Weekly warning margin: 0 (off: no early warnings or Slack warning; park explanations stay)"
	}
	return "Weekly warning margin: " + strconv.Itoa(pts) + " points"
}
