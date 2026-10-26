package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// newSettingsReserveCmd is `uzi settings reserve` (PRD #3522): set the weekly usage
// reserve, the 7-day utilization percent at which uzi stops spending your subscription.
func newSettingsReserveCmd(env Env, gf *globalFlags) *cobra.Command {
	var threshold string
	var pauseRunning bool
	cmd := &cobra.Command{
		Use:   "reserve",
		Short: "Set the weekly usage reserve",
		Long: "Set the weekly usage reserve: the 7-day utilization percent at which uzi stops\n" +
			"starting runs on your subscription (and, with --pause-running, pauses running runs at\n" +
			"their next milestone). --threshold takes 1 to 100, or off to turn the reserve off.\n\n" +
			"--pause-running (or --pause-running=false) sets whether running runs are also paused.\n" +
			"When you omit it, the current value is kept.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			pct, err := parseReserveThreshold(threshold)
			if err != nil {
				return uzicli.Exitf(uzicli.ExitUsage, "%v", err)
			}
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			pause := pauseRunning
			if !cmd.Flags().Changed("pause-running") {
				// The server requires both fields, so an omitted flag keeps the stored value.
				current, err := c.GetMySettings(cmd.Context())
				if err != nil {
					return err
				}
				pause = current.WeeklyReservePauseRunning
			}
			user, err := c.SetUsageReserve(cmd.Context(), pct, pause)
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(map[string]any{
					"weekly_reserve_pct":           user.WeeklyReservePct,
					"weekly_reserve_pause_running": user.WeeklyReservePauseRunning,
				})
			}
			p.Println(reserveLine(user.WeeklyReservePct, user.WeeklyReservePauseRunning))
			return nil
		},
	}
	cmd.Flags().StringVar(&threshold, "threshold", "", "7-day utilization percent (1 to 100) at which uzi stops spending your subscription, or off")
	cmd.Flags().BoolVar(&pauseRunning, "pause-running", true, "also pause running runs at their next milestone (omit to keep the current value)")
	cmd.Flags().Lookup("pause-running").NoOptDefVal = "true"
	_ = cmd.MarkFlagRequired("threshold")
	return cmd
}

// parseReserveThreshold maps "off" to nil and "1".."100" to a pointer; anything else is an
// error. The server revalidates, so this only gives the early, readable message.
func parseReserveThreshold(s string) (*int, error) {
	if strings.EqualFold(strings.TrimSpace(s), "off") {
		return nil, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 1 || n > 100 {
		return nil, fmt.Errorf("invalid --threshold %q: use an integer from 1 to 100, or off", s)
	}
	return &n, nil
}

// reserveLine renders the weekly usage reserve for `settings get` and `settings reserve`.
func reserveLine(pct *int, pauseRunning bool) string {
	if pct == nil {
		return "Weekly usage reserve: off"
	}
	pause := "off"
	if pauseRunning {
		pause = "on"
	}
	return "Weekly usage reserve: " + strconv.Itoa(*pct) + "% (pause running runs: " + pause + ")"
}

func settingsReserveLine(s apitypes.UserSettingsDTO) string {
	return reserveLine(s.WeeklyReservePct, s.WeeklyReservePauseRunning)
}
